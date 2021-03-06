// +build linux darwin freebsd
// +build !nofuse

package readonly

import (
	core "github.com/ipfs/go-ipfs/core"
	mount "github.com/ipfs/go-ipfs/fuse/mount"
)

// Mount mounts ipfs at a given location, and returns a mount.Mount instance.
func Mount(ipfs *core.IpfsNode, mountpoint string) (mount.Mount, error) {
	cfg := ipfs.Repo.Config()
	allow_other := cfg.Mounts.FuseAllowOther
	fsys := NewFileSystem(ipfs)
	return mount.NewMount(ipfs.Process(), fsys, mountpoint, allow_other)
}
