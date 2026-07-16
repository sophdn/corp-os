package session

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// TreeNode is one session in the sub-orchestration tree: its header (which
// carries the profile/duty and the parent edge), its own rolled-up telemetry,
// and its spawned-worker children. It is the unit the swap's run-rate signal is
// computed over — the whole tree, not just the root session (T6).
type TreeNode struct {
	Header      Header
	CostUSD     float64
	Turns       int
	ToolCalls   int
	Escalations []Escalation
	Children    []*TreeNode
}

// TreeCostUSD is this node's cost plus every descendant's — the run-rate over the
// full decomposition tree.
func (n *TreeNode) TreeCostUSD() float64 {
	total := n.CostUSD
	for _, c := range n.Children {
		total += c.TreeCostUSD()
	}
	return total
}

// Size is the number of sessions in the (sub)tree rooted at this node.
func (n *TreeNode) Size() int {
	count := 1
	for _, c := range n.Children {
		count += c.Size()
	}
	return count
}

// loadNode opens one session DB and reads its header plus rolled-up telemetry.
func loadNode(dir, runID string) (*TreeNode, error) {
	st, err := Open(dir, runID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }()
	h, err := st.HeaderRow()
	if err != nil {
		return nil, err
	}
	cost, err := st.CostTotal()
	if err != nil {
		return nil, err
	}
	turns, toolCalls, err := st.Counts()
	if err != nil {
		return nil, err
	}
	esc, err := st.Escalations()
	if err != nil {
		return nil, err
	}
	return &TreeNode{Header: h, CostUSD: cost, Turns: turns, ToolCalls: toolCalls, Escalations: esc}, nil
}

// LoadTree reconstructs the sub-orchestration tree rooted at rootRunID by reading
// every session DB under dir and linking them by parent_run_id. Sessions that
// can't be opened (a different schema version, a corrupt file) are skipped rather
// than failing the whole tree. Children are ordered by run id (time-sortable), so
// the rendering is stable and chronological.
func LoadTree(dir, rootRunID string) (*TreeNode, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.db"))
	if err != nil {
		return nil, err
	}
	nodes := make(map[string]*TreeNode, len(files))
	childrenOf := map[string][]string{}
	for _, f := range files {
		runID := strings.TrimSuffix(filepath.Base(f), ".db")
		n, nerr := loadNode(dir, runID)
		if nerr != nil {
			continue // skip unreadable / old-version DBs
		}
		nodes[runID] = n
		if p := n.Header.ParentRunID; p != "" {
			childrenOf[p] = append(childrenOf[p], runID)
		}
	}
	root, ok := nodes[rootRunID]
	if !ok {
		return nil, fmt.Errorf("no session %q under %s", rootRunID, dir)
	}
	var attach func(*TreeNode)
	attach = func(parent *TreeNode) {
		kids := childrenOf[parent.Header.RunID]
		sort.Strings(kids)
		for _, cid := range kids {
			c := nodes[cid]
			parent.Children = append(parent.Children, c)
			attach(c)
		}
	}
	attach(root)
	return root, nil
}
