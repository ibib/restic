// +build !openbsd
// +build !windows

package fuse

import (
	"os"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"golang.org/x/net/context"

	"restic"
	"restic/debug"
)

// Statically ensure that *dir implement those interface
var _ = fs.HandleReadDirAller(&dir{})
var _ = fs.NodeStringLookuper(&dir{})

type dir struct {
	repo        restic.Repository
	items       map[string]*restic.Node
	inode       uint64
	node        *restic.Node
	ownerIsRoot bool

	blobsize *BlobSizeCache
}

func newDir(ctx context.Context, repo restic.Repository, node *restic.Node, ownerIsRoot bool, blobsize *BlobSizeCache) (*dir, error) {
	debug.Log("new dir for %v (%v)", node.Name, node.Subtree.Str())
	tree, err := repo.LoadTree(ctx, *node.Subtree)
	if err != nil {
		debug.Log("  error loading tree %v: %v", node.Subtree.Str(), err)
		return nil, err
	}
	items := make(map[string]*restic.Node)
	for _, node := range tree.Nodes {
		items[node.Name] = node
	}

	return &dir{
		repo:        repo,
		node:        node,
		items:       items,
		inode:       node.Inode,
		ownerIsRoot: ownerIsRoot,
		blobsize:    blobsize,
	}, nil
}

// replaceSpecialNodes replaces nodes with name "." and "/" by their contents.
// Otherwise, the node is returned.
func replaceSpecialNodes(ctx context.Context, repo restic.Repository, node *restic.Node) ([]*restic.Node, error) {
	if node.Type != "dir" || node.Subtree == nil {
		return []*restic.Node{node}, nil
	}

	if node.Name != "." && node.Name != "/" {
		return []*restic.Node{node}, nil
	}

	tree, err := repo.LoadTree(ctx, *node.Subtree)
	if err != nil {
		return nil, err
	}

	return tree.Nodes, nil
}

func newDirFromSnapshot(ctx context.Context, repo restic.Repository, snapshot SnapshotWithId, ownerIsRoot bool, blobsize *BlobSizeCache) (*dir, error) {
	debug.Log("new dir for snapshot %v (%v)", snapshot.ID.Str(), snapshot.Tree.Str())
	tree, err := repo.LoadTree(ctx, *snapshot.Tree)
	if err != nil {
		debug.Log("  loadTree(%v) failed: %v", snapshot.ID.Str(), err)
		return nil, err
	}
	items := make(map[string]*restic.Node)
	for _, n := range tree.Nodes {
		nodes, err := replaceSpecialNodes(ctx, repo, n)
		if err != nil {
			debug.Log("  replaceSpecialNodes(%v) failed: %v", n, err)
			return nil, err
		}

		for _, node := range nodes {
			items[node.Name] = node
		}
	}

	return &dir{
		repo: repo,
		node: &restic.Node{
			UID:        uint32(os.Getuid()),
			GID:        uint32(os.Getgid()),
			AccessTime: snapshot.Time,
			ModTime:    snapshot.Time,
			ChangeTime: snapshot.Time,
			Mode:       os.ModeDir | 0555,
		},
		items:       items,
		inode:       inodeFromBackendID(snapshot.ID),
		ownerIsRoot: ownerIsRoot,
		blobsize:    blobsize,
	}, nil
}

func (d *dir) Attr(ctx context.Context, a *fuse.Attr) error {
	debug.Log("called")
	a.Inode = d.inode
	a.Mode = os.ModeDir | d.node.Mode

	if !d.ownerIsRoot {
		a.Uid = d.node.UID
		a.Gid = d.node.GID
	}
	a.Atime = d.node.AccessTime
	a.Ctime = d.node.ChangeTime
	a.Mtime = d.node.ModTime

	a.Nlink = d.calcNumberOfLinks()

	return nil
}

func (d *dir) calcNumberOfLinks() uint32 {
	// a directory d has 2 hardlinks + the number
	// of directories contained by d
	var count uint32
	count = 2
	for _, node := range d.items {
		if node.Type == "dir" {
			count++
		}
	}
	return count
}

func (d *dir) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	debug.Log("called")
	ret := make([]fuse.Dirent, 0, len(d.items))

	for _, node := range d.items {
		var typ fuse.DirentType
		switch node.Type {
		case "dir":
			typ = fuse.DT_Dir
		case "file":
			typ = fuse.DT_File
		case "symlink":
			typ = fuse.DT_Link
		}

		ret = append(ret, fuse.Dirent{
			Inode: node.Inode,
			Type:  typ,
			Name:  node.Name,
		})
	}

	return ret, nil
}

func (d *dir) Lookup(ctx context.Context, name string) (fs.Node, error) {
	debug.Log("Lookup(%v)", name)
	node, ok := d.items[name]
	if !ok {
		debug.Log("  Lookup(%v) -> not found", name)
		return nil, fuse.ENOENT
	}
	switch node.Type {
	case "dir":
		return newDir(ctx, d.repo, node, d.ownerIsRoot, d.blobsize)
	case "file":
		return newFile(d.repo, node, d.ownerIsRoot, d.blobsize)
	case "symlink":
		return newLink(d.repo, node, d.ownerIsRoot)
	default:
		debug.Log("  node %v has unknown type %v", name, node.Type)
		return nil, fuse.ENOENT
	}
}

func (d *dir) Listxattr(ctx context.Context, req *fuse.ListxattrRequest, resp *fuse.ListxattrResponse) error {
	debug.Log("Listxattr(%v, %v)", d.node.Name, req.Size)
	for _, attr := range d.node.ExtendedAttributes {
		resp.Append(attr.Name)
	}
	return nil
}

func (d *dir) Getxattr(ctx context.Context, req *fuse.GetxattrRequest, resp *fuse.GetxattrResponse) error {
	debug.Log("Getxattr(%v, %v, %v)", d.node.Name, req.Name, req.Size)
	attrval := d.node.GetExtendedAttribute(req.Name)
	if attrval != nil {
		resp.Xattr = attrval
		return nil
	}
	return fuse.ErrNoXattr
}
