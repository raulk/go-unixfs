// Package trickle allows to build trickle DAGs.
// In this type of DAG, non-leave nodes are first filled
// with data leaves, and then incorporate "layers" of subtrees
// as additional links.
//
// Each layer is a trickle sub-tree and is limited by an increasing
// maximum depth. Thus, the nodes first layer
// can only hold leaves (depth 1) but subsequent layers can grow deeper.
// By default, this module places 4 nodes per layer (that is, 4 subtrees
// of the same maximum depth before increasing it).
//
// Trickle DAGs are very good for sequentially reading data, as the
// first data leaves are directly reachable from the root and those
// coming next are always nearby. They are
// suited for things like streaming applications.
package trickle

import (
	"context"
	"errors"
	"fmt"

	ft "github.com/ipfs/go-unixfs"
	h "github.com/ipfs/go-unixfs/importer/helpers"

	cid "github.com/ipfs/go-cid"
	ipld "github.com/ipfs/go-ipld-format"
	dag "github.com/ipfs/go-merkledag"
)

// depthRepeat specifies how many times to append a child tree of a
// given depth. Higher values increase the width of a given node, which
// improves seek speeds.
const depthRepeat = 4

// Layout builds a new DAG with the trickle format using the provided
// DagBuilderHelper. See the module's description for a more detailed
// explanation.
func Layout(db *h.DagBuilderHelper) (ipld.Node, error) {
	newRoot := db.NewFSNodeOverDag(ft.TFile)
	root, _, err := fillTrickleRec(db, newRoot, -1)
	if err != nil {
		return nil, err
	}

	return root, db.Add(root)
}

// fillTrickleRec creates a trickle (sub-)tree with an optional maximum specified depth
// in the case maxDepth is greater than zero, or with unlimited depth otherwise
// (where the DAG builder will signal the end of data to end the function).
func fillTrickleRec(db *h.DagBuilderHelper, node *h.FSNodeOverDag, maxDepth int) (filledNode ipld.Node, nodeFileSize uint64, err error) {
	// Always do this, even in the base case
	if err := db.FillNodeLayer(node); err != nil {
		return nil, 0, err
	}

	// For each depth in [1, `maxDepth`) (or without limit if `maxDepth` is -1,
	// initial call from `Layout`) add `depthRepeat` sub-graphs of that depth.
	for depth := 1; maxDepth == -1 || depth < maxDepth; depth++ {
		if db.Done() {
			break
			// No more data, stop here, posterior append calls will figure out
			// where we left off.
		}

		for repeatIndex := 0; repeatIndex < depthRepeat && !db.Done(); repeatIndex++ {

			childNode, childFileSize, err := fillTrickleRec(db, db.NewFSNodeOverDag(ft.TFile), depth)
			if err != nil {
				return nil, 0, err
			}

			if err := node.AddChild(childNode, childFileSize, db); err != nil {
				return nil, 0, err
			}
		}
	}

	// Get the final `dag.ProtoNode` with the `FSNode` data encoded inside.
	filledNode, err = node.Commit()
	if err != nil {
		return nil, 0, err
	}

	return filledNode, node.FileSize(), nil
}

// Append appends the data in `db` to the dag, using the Trickledag format
func Append(ctx context.Context, basen ipld.Node, db *h.DagBuilderHelper) (out ipld.Node, errOut error) {
	base, ok := basen.(*dag.ProtoNode)
	if !ok {
		return nil, dag.ErrNotProtobuf
	}

	// Convert to unixfs node for working with easily

	fsn, err := h.NewFSNFromDag(base)
	if err != nil {
		return nil, err
	}

	// Get depth of this 'tree'
	n, layerProgress := trickleDepthInfo(fsn, db.Maxlinks())
	if n == 0 {
		// If direct blocks not filled...
		if err := db.FillNodeLayer(fsn); err != nil {
			return nil, err
		}

		if db.Done() {
			return fsn.GetDagNode()
		}

		// If continuing, our depth has increased by one
		n++
	}

	// Last child in this node may not be a full tree, lets file it up
	if err := appendFillLastChild(ctx, fsn, n-1, layerProgress, db); err != nil {
		return nil, err
	}

	// after appendFillLastChild, our depth is now increased by one
	if !db.Done() {
		n++
	}

	// Now, continue filling out tree like normal
	for i := n; !db.Done(); i++ {
		for j := 0; j < depthRepeat && !db.Done(); j++ {
			nextChild := db.NewFSNodeOverDag(ft.TFile)
			childNode, childFileSize, err := fillTrickleRec(db, nextChild, i)
			if err != nil {
				return nil, err
			}
			err = fsn.AddChild(childNode, childFileSize, db)
			if err != nil {
				return nil, err
			}
		}
	}
	_, err = fsn.Commit()
	if err != nil {
		return nil, err
	}
	return fsn.GetDagNode()
}

func appendFillLastChild(ctx context.Context, fsn *h.FSNodeOverDag, depth int, layerFill int, db *h.DagBuilderHelper) error {
	if fsn.NumChildren() <= db.Maxlinks() {
		return nil
	}
	// Recursive step, grab last child
	last := fsn.NumChildren() - 1
	lastChild, err := fsn.GetChild(ctx, last, db.GetDagServ())
	if err != nil {
		return err
	}

	// Fill out last child (may not be full tree)
	nchild, nchildSize, err := appendRec(ctx, lastChild, db, depth-1)
	if err != nil {
		return err
	}

	// Update changed child in parent node
	fsn.RemoveChild(last, db)
	filledNode, err := nchild.Commit()
	if err != nil {
		return err
	}
	err = fsn.AddChild(filledNode, nchildSize, db)
	if err != nil {
		return err
	}

	// Partially filled depth layer
	if layerFill != 0 {
		for ; layerFill < depthRepeat && !db.Done(); layerFill++ {
			nextChild := db.NewFSNodeOverDag(ft.TFile)
			childNode, childFileSize, err := fillTrickleRec(db, nextChild, depth)
			if err != nil {
				return err
			}

			if err := fsn.AddChild(childNode, childFileSize, db); err != nil {
				return err
			}
		}
	}

	return nil
}

// recursive call for Append
func appendRec(ctx context.Context, fsn *h.FSNodeOverDag, db *h.DagBuilderHelper, depth int) (*h.FSNodeOverDag, uint64, error) {
	if depth == 0 || db.Done() {
		return fsn, fsn.FileSize(), nil
	}

	// Get depth of this 'tree'
	n, layerProgress := trickleDepthInfo(fsn, db.Maxlinks())
	if n == 0 {
		// If direct blocks not filled...
		if err := db.FillNodeLayer(fsn); err != nil {
			return nil, 0, err
		}
		n++
	}

	// If at correct depth, no need to continue
	if n == depth {
		return fsn, fsn.FileSize(), nil
	}

	if err := appendFillLastChild(ctx, fsn, n, layerProgress, db); err != nil {
		return nil, 0, err
	}

	// after appendFillLastChild, our depth is now increased by one
	if !db.Done() {
		n++
	}

	// Now, continue filling out tree like normal
	for i := n; i < depth && !db.Done(); i++ {
		for j := 0; j < depthRepeat && !db.Done(); j++ {
			nextChild := db.NewFSNodeOverDag(ft.TFile)
			childNode, childFileSize, err := fillTrickleRec(db, nextChild, i)
			if err != nil {
				return nil, 0, err
			}

			if err := fsn.AddChild(childNode, childFileSize, db); err != nil {
				return nil, 0, err
			}
		}
	}

	return fsn, fsn.FileSize(), nil
}

// Deduce where we left off in `fillTrickleRec`, returns the `depth`
// with which new sub-graphs were being added and, within that depth,
// in which `repeatNumber` of the total `depthRepeat` we should add.
func trickleDepthInfo(node *h.FSNodeOverDag, maxlinks int) (depth int, repeatNumber int) {
	n := node.NumChildren()

	if n < maxlinks {
		// We didn't even added the initial `maxlinks` leaf nodes (`FillNodeLayer`).
		return 0, 0
	}

	nonLeafChildren := n - maxlinks
	// The number of non-leaf child nodes added in `fillTrickleRec` (after
	// the `FillNodeLayer` call).

	depth = nonLeafChildren/depthRepeat + 1
	// "Deduplicate" the added `depthRepeat` sub-graphs at each depth
	// (rounding it up since we may be on an unfinished depth with less
	// than `depthRepeat` sub-graphs).

	repeatNumber = nonLeafChildren % depthRepeat
	// What's left after taking full depths of `depthRepeat` sub-graphs
	// is the current `repeatNumber` we're at (this fractional part is
	// what we rounded up before).

	return
}

// VerifyParams is used by VerifyTrickleDagStructure
type VerifyParams struct {
	Getter      ipld.NodeGetter
	Direct      int
	LayerRepeat int
	Prefix      *cid.Prefix
	RawLeaves   bool
}

// VerifyTrickleDagStructure checks that the given dag matches exactly the trickle dag datastructure
// layout
func VerifyTrickleDagStructure(nd ipld.Node, p VerifyParams) error {
	return verifyTDagRec(nd, -1, p)
}

// Recursive call for verifying the structure of a trickledag
func verifyTDagRec(n ipld.Node, depth int, p VerifyParams) error {
	codec := cid.DagProtobuf
	if depth == 0 {
		if len(n.Links()) > 0 {
			return errors.New("expected direct block")
		}
		// zero depth dag is raw data block
		switch nd := n.(type) {
		case *dag.ProtoNode:
			fsn, err := ft.FSNodeFromBytes(nd.Data())
			if err != nil {
				return err
			}

			if fsn.Type() != ft.TRaw {
				return errors.New("expected raw block")
			}

			if p.RawLeaves {
				return errors.New("expected raw leaf, got a protobuf node")
			}
		case *dag.RawNode:
			if !p.RawLeaves {
				return errors.New("expected protobuf node as leaf")
			}
			codec = cid.Raw
		default:
			return errors.New("expected ProtoNode or RawNode")
		}
	}

	// verify prefix
	if p.Prefix != nil {
		prefix := n.Cid().Prefix()
		expect := *p.Prefix // make a copy
		expect.Codec = uint64(codec)
		if codec == cid.Raw && expect.Version == 0 {
			expect.Version = 1
		}
		if expect.MhLength == -1 {
			expect.MhLength = prefix.MhLength
		}
		if prefix != expect {
			return fmt.Errorf("unexpected cid prefix: expected: %v; got %v", expect, prefix)
		}
	}

	if depth == 0 {
		return nil
	}

	nd, ok := n.(*dag.ProtoNode)
	if !ok {
		return errors.New("expected ProtoNode")
	}

	// Verify this is a branch node
	fsn, err := ft.FSNodeFromBytes(nd.Data())
	if err != nil {
		return err
	}

	if fsn.Type() != ft.TFile {
		return fmt.Errorf("expected file as branch node, got: %s", fsn.Type())
	}

	if len(fsn.Data()) > 0 {
		return errors.New("branch node should not have data")
	}

	for i := 0; i < len(nd.Links()); i++ {
		child, err := nd.Links()[i].GetNode(context.TODO(), p.Getter)
		if err != nil {
			return err
		}

		if i < p.Direct {
			// Direct blocks
			err := verifyTDagRec(child, 0, p)
			if err != nil {
				return err
			}
		} else {
			// Recursive trickle dags
			rdepth := ((i - p.Direct) / p.LayerRepeat) + 1
			if rdepth >= depth && depth > 0 {
				return errors.New("child dag was too deep")
			}
			err := verifyTDagRec(child, rdepth, p)
			if err != nil {
				return err
			}
		}
	}
	return nil
}
