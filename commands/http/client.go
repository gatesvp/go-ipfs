package http

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	cmds "github.com/ipfs/go-ipfs/commands"
	config "github.com/ipfs/go-ipfs/repo/config"

	context "github.com/ipfs/go-ipfs/Godeps/_workspace/src/golang.org/x/net/context"
)

const (
	ApiUrlFormat = "http://%s%s/%s?%s"
	ApiPath      = "/api/v0" // TODO: make configurable
)

// Client is the commands HTTP client interface.
type Client interface {
	Send(req cmds.Request) (cmds.Response, error)
}

type client struct {
	serverAddress string
	httpClient    http.Client
}

func NewClient(address string) Client {
	// We cannot use the default transport because of a bug in go's connection reuse
	// code. It causes random failures in the connection including io.EOF and connection
	// refused on 'client.Do'
	return &client{
		serverAddress: address,
		httpClient: http.Client{
			Transport: &http.Transport{
				DisableKeepAlives: true,
			},
		},
	}
}

func (c *client) Send(req cmds.Request) (cmds.Response, error) {

	if req.Context() == nil {
		log.Warningf("no context set in request")
		err := req.SetRootContext(context.TODO())
		if err != nil {
			return nil, err
		}
	}

	// save user-provided encoding
	previousUserProvidedEncoding, found, err := req.Option(cmds.EncShort).String()
	if err != nil {
		return nil, err
	}

	// override with json to send to server
	req.SetOption(cmds.EncShort, cmds.JSON)

	// stream channel output
	req.SetOption(cmds.ChanOpt, "true")

	query, err := getQuery(req)
	if err != nil {
		return nil, err
	}

	var fileReader *MultiFileReader
	var reader io.Reader

	if req.Files() != nil {
		fileReader = NewMultiFileReader(req.Files(), true)
		reader = fileReader
	} else {
		// if we have no file data, use an empty Reader
		// (http.NewRequest panics when a nil Reader is used)
		reader = strings.NewReader("")
	}

	path := strings.Join(req.Path(), "/")
	url := fmt.Sprintf(ApiUrlFormat, c.serverAddress, ApiPath, path, query)

	httpReq, err := http.NewRequest("POST", url, reader)
	if err != nil {
		return nil, err
	}

	// TODO extract string consts?
	if fileReader != nil {
		httpReq.Header.Set(contentTypeHeader, "multipart/form-data; boundary="+fileReader.Boundary())
		httpReq.Header.Set(contentDispHeader, "form-data: name=\"files\"")
	} else {
		httpReq.Header.Set(contentTypeHeader, applicationOctetStream)
	}
	version := config.CurrentVersionNumber
	httpReq.Header.Set(uaHeader, fmt.Sprintf("/go-ipfs/%s/", version))

	ec := make(chan error, 1)
	rc := make(chan cmds.Response, 1)
	dc := req.Context().Done()

	go func() {
		httpRes, err := c.httpClient.Do(httpReq)
		if err != nil {
			ec <- err
			return
		}

		// using the overridden JSON encoding in request
		res, err := getResponse(httpRes, req)
		if err != nil {
			ec <- err
			return
		}

		rc <- res
	}()

	for {
		select {
		case <-dc:
			log.Debug("Context cancelled, cancelling HTTP request...")
			tr := http.DefaultTransport.(*http.Transport)
			tr.CancelRequest(httpReq)
			dc = nil // Wait for ec or rc
		case err := <-ec:
			return nil, err
		case res := <-rc:
			if found && len(previousUserProvidedEncoding) > 0 {
				// reset to user provided encoding after sending request
				// NB: if user has provided an encoding but it is the empty string,
				// still leave it as JSON.
				req.SetOption(cmds.EncShort, previousUserProvidedEncoding)
			}
			return res, nil
		}
	}
}

func getQuery(req cmds.Request) (string, error) {
	query := url.Values{}
	for k, v := range req.Options() {
		str := fmt.Sprintf("%v", v)
		query.Set(k, str)
	}

	args := req.Arguments()
	argDefs := req.Command().Arguments

	argDefIndex := 0

	for _, arg := range args {
		argDef := argDefs[argDefIndex]
		// skip ArgFiles
		for argDef.Type == cmds.ArgFile {
			argDefIndex++
			argDef = argDefs[argDefIndex]
		}

		query.Add("arg", arg)

		if len(argDefs) > argDefIndex+1 {
			argDefIndex++
		}
	}

	return query.Encode(), nil
}

// getResponse decodes a http.Response to create a cmds.Response
func getResponse(httpRes *http.Response, req cmds.Request) (cmds.Response, error) {
	var err error
	res := cmds.NewResponse(req)

	contentType := httpRes.Header.Get(contentTypeHeader)
	contentType = strings.Split(contentType, ";")[0]

	lengthHeader := httpRes.Header.Get(contentLengthHeader)
	if len(lengthHeader) > 0 {
		length, err := strconv.ParseUint(lengthHeader, 10, 64)
		if err != nil {
			return nil, err
		}
		res.SetLength(length)
	}

	rr := &httpResponseReader{httpRes}
	res.SetCloser(rr)

	if contentType != applicationJson {
		// for all non json output types, just stream back the output
		res.SetOutput(rr)
		return res, nil

	} else if len(httpRes.Header.Get(channelHeader)) > 0 {
		// if output is coming from a channel, decode each chunk
		outChan := make(chan interface{})

		go readStreamedJson(req, rr, outChan)

		res.SetOutput((<-chan interface{})(outChan))
		return res, nil
	}

	dec := json.NewDecoder(rr)

	// If we ran into an error
	if httpRes.StatusCode >= http.StatusBadRequest {
		e := cmds.Error{}

		switch {
		case httpRes.StatusCode == http.StatusNotFound:
			// handle 404s
			e.Message = "Command not found."
			e.Code = cmds.ErrClient

		case contentType == plainText:
			// handle non-marshalled errors
			buf := bytes.NewBuffer(nil)
			io.Copy(buf, httpRes.Body)
			e.Message = string(buf.Bytes())
			e.Code = cmds.ErrNormal

		default:
			// handle marshalled errors
			err = dec.Decode(&e)
			if err != nil {
				return nil, err
			}
		}

		res.SetError(e, e.Code)

		return res, nil
	}

	outputType := reflect.TypeOf(req.Command().Type)
	v, err := decodeTypedVal(outputType, dec)
	if err != nil && err != io.EOF {
		return nil, err
	}

	res.SetOutput(v)

	return res, nil
}

// read json objects off of the given stream, and write the objects out to
// the 'out' channel
func readStreamedJson(req cmds.Request, rr io.Reader, out chan<- interface{}) {
	defer close(out)
	dec := json.NewDecoder(rr)
	outputType := reflect.TypeOf(req.Command().Type)

	ctx := req.Context()

	for {
		v, err := decodeTypedVal(outputType, dec)
		if err != nil {
			// reading on a closed response body is as good as an io.EOF here
			if !(strings.Contains(err.Error(), "read on closed response body") || err == io.EOF) {
				log.Error(err)
			}
			return
		}

		select {
		case <-ctx.Done():
			return
		case out <- v:
		}
	}
}

// decode a value of the given type, if the type is nil, attempt to decode into
// an interface{} anyways
func decodeTypedVal(t reflect.Type, dec *json.Decoder) (interface{}, error) {
	var v interface{}
	var err error
	if t != nil {
		v = reflect.New(t).Interface()
		err = dec.Decode(v)
	} else {
		err = dec.Decode(&v)
	}

	return v, err
}

// httpResponseReader reads from the response body, and checks for an error
// in the http trailer upon EOF, this error if present is returned instead
// of the EOF.
type httpResponseReader struct {
	resp *http.Response
}

func (r *httpResponseReader) Read(b []byte) (int, error) {
	n, err := r.resp.Body.Read(b)
	if err == io.EOF {
		_ = r.resp.Body.Close()
		trailerErr := r.checkError()
		if trailerErr != nil {
			return n, trailerErr
		}
	}
	return n, err
}

func (r *httpResponseReader) checkError() error {
	if e := r.resp.Trailer.Get(StreamErrHeader); e != "" {
		return errors.New(e)
	}
	return nil
}

func (r *httpResponseReader) Close() error {
	return r.resp.Body.Close()
}
