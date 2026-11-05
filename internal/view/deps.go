package view

import (
	"fmt"
	"html/template"
	"io/fs"
	"slices"
	texttemplate "text/template"
	"text/template/parse"
)

// sharedFile is a layout or partial: its source, the templates it defines
// and the templates it calls.
type sharedFile struct {
	path    string
	src     string
	defines []string
	calls   []string
}

// sharedFiles reads layouts/*.html then partials/*.html (the order ParseFS
// used, so a redefined name still resolves the same way) and indexes which
// files define each template name.
func sharedFiles(fsys fs.FS, funcs template.FuncMap) ([]sharedFile, map[string][]int, error) {
	var files []sharedFile
	for _, pattern := range []string{"layouts/*.html", "partials/*.html"} {
		paths, err := fs.Glob(fsys, pattern)
		if err != nil {
			return nil, nil, err
		}
		slices.Sort(paths)
		for _, p := range paths {
			src, err := fs.ReadFile(fsys, p)
			if err != nil {
				return nil, nil, err
			}
			defines, calls, err := scanTemplates(p, string(src), funcs)
			if err != nil {
				return nil, nil, fmt.Errorf("view: parse %s: %w", p, err)
			}
			files = append(files, sharedFile{path: p, src: string(src), defines: defines, calls: calls})
		}
	}
	definedIn := map[string][]int{}
	for i, f := range files {
		for _, name := range f.defines {
			definedIn[name] = append(definedIn[name], i)
		}
	}
	return files, definedIn, nil
}

// closure returns, in file order, the shared files needed to resolve the
// given template calls and everything those files call in turn.
func closure(calls []string, files []sharedFile, definedIn map[string][]int) []int {
	need := map[int]bool{}
	seen := map[string]bool{}
	queue := slices.Clone(calls)
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if seen[name] {
			continue
		}
		seen[name] = true
		for _, i := range definedIn[name] {
			if !need[i] {
				need[i] = true
				queue = append(queue, files[i].calls...)
			}
		}
	}
	out := make([]int, 0, len(need))
	for i := range need {
		out = append(out, i)
	}
	slices.Sort(out)
	return out
}

// scanTemplates parses src (text/template syntax, with the renderer's
// functions) and lists the templates it defines and the names it calls.
func scanTemplates(name, src string, funcs template.FuncMap) (defines, calls []string, err error) {
	t, err := texttemplate.New(name).Funcs(texttemplate.FuncMap(funcs)).Parse(src)
	if err != nil {
		return nil, nil, err
	}
	called := map[string]bool{}
	for _, d := range t.Templates() {
		if d.Name() != name {
			defines = append(defines, d.Name())
		}
		if d.Tree != nil {
			walkCalls(d.Tree.Root, called)
		}
	}
	for c := range called {
		calls = append(calls, c)
	}
	slices.Sort(calls)
	return defines, calls, nil
}

func walkCalls(n parse.Node, out map[string]bool) {
	switch n := n.(type) {
	case *parse.ListNode:
		if n == nil {
			return
		}
		for _, c := range n.Nodes {
			walkCalls(c, out)
		}
	case *parse.TemplateNode:
		out[n.Name] = true
	case *parse.IfNode:
		walkCalls(n.List, out)
		walkCalls(n.ElseList, out)
	case *parse.RangeNode:
		walkCalls(n.List, out)
		walkCalls(n.ElseList, out)
	case *parse.WithNode:
		walkCalls(n.List, out)
		walkCalls(n.ElseList, out)
	}
}
