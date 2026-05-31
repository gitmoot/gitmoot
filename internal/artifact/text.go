package artifact

import (
	"bytes"
	"fmt"
	"strings"
)

const (
	DefaultPreviewLines = 80
)

type TextDriver struct {
	PreviewLines int
}

func (d TextDriver) Preview(content []byte) string {
	limit := d.PreviewLines
	if limit <= 0 {
		limit = DefaultPreviewLines
	}
	lines := splitLines(content)
	if len(lines) <= limit {
		return strings.Join(lines, "\n")
	}
	preview := append([]string{}, lines[:limit]...)
	preview = append(preview, fmt.Sprintf("... %d more lines", len(lines)-limit))
	return strings.Join(preview, "\n")
}

func (d TextDriver) Diff(oldName, newName string, oldContent, newContent []byte) string {
	oldLines := splitLines(oldContent)
	newLines := splitLines(newContent)
	ops := diffLines(oldLines, newLines)
	var out bytes.Buffer
	if strings.TrimSpace(oldName) == "" {
		oldName = "a"
	}
	if strings.TrimSpace(newName) == "" {
		newName = "b"
	}
	fmt.Fprintf(&out, "--- %s\n", oldName)
	fmt.Fprintf(&out, "+++ %s\n", newName)
	for _, op := range ops {
		switch op.kind {
		case equalLine:
			fmt.Fprintf(&out, " %s\n", op.text)
		case deleteLine:
			fmt.Fprintf(&out, "-%s\n", op.text)
		case insertLine:
			fmt.Fprintf(&out, "+%s\n", op.text)
		}
	}
	return out.String()
}

func splitLines(content []byte) []string {
	normalized := strings.ReplaceAll(string(content), "\r\n", "\n")
	normalized = strings.TrimSuffix(normalized, "\n")
	if normalized == "" {
		return []string{}
	}
	return strings.Split(normalized, "\n")
}

type lineKind int

const (
	equalLine lineKind = iota
	deleteLine
	insertLine
)

type lineOp struct {
	kind lineKind
	text string
}

func diffLines(oldLines, newLines []string) []lineOp {
	table := make([][]int, len(oldLines)+1)
	for i := range table {
		table[i] = make([]int, len(newLines)+1)
	}
	for i := len(oldLines) - 1; i >= 0; i-- {
		for j := len(newLines) - 1; j >= 0; j-- {
			if oldLines[i] == newLines[j] {
				table[i][j] = table[i+1][j+1] + 1
			} else if table[i+1][j] >= table[i][j+1] {
				table[i][j] = table[i+1][j]
			} else {
				table[i][j] = table[i][j+1]
			}
		}
	}
	var ops []lineOp
	i, j := 0, 0
	for i < len(oldLines) && j < len(newLines) {
		if oldLines[i] == newLines[j] {
			ops = append(ops, lineOp{kind: equalLine, text: oldLines[i]})
			i++
			j++
		} else if table[i+1][j] >= table[i][j+1] {
			ops = append(ops, lineOp{kind: deleteLine, text: oldLines[i]})
			i++
		} else {
			ops = append(ops, lineOp{kind: insertLine, text: newLines[j]})
			j++
		}
	}
	for i < len(oldLines) {
		ops = append(ops, lineOp{kind: deleteLine, text: oldLines[i]})
		i++
	}
	for j < len(newLines) {
		ops = append(ops, lineOp{kind: insertLine, text: newLines[j]})
		j++
	}
	return ops
}
