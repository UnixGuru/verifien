package main

// include and sinclude: pull other config files (a path or an http(s) URL)
// into this one before anything is validated.
//
//	include  "web.cfg"                       a missing file is an error
//	sinclude "local.cfg"                     a missing file is skipped silently
//	include  "https://example.com/team.cfg"
//
// The included text is spliced in where the include stands, so its sections run
// as if they had been written there. Its [DEFAULTS] and [ACTION_SCRIPTS] are
// merged with the including file's (see expand).

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alecthomas/participle/v2"
	"github.com/alecthomas/participle/v2/lexer"
)

const (
	includeTimeout = 10 * time.Second // per URL; [DEFAULTS] is not known yet while includes are read
	includeMaxSize = 1 << 20          // bytes
)

// Include is `include "path-or-url"` or `sinclude "path-or-url"`.
type Include struct {
	Pos  lexer.Position
	Kw   string `@Include`
	Path string `@String`
}

func (i *Include) optional() bool { return i.Kw == "sinclude" }

func isURL(s string) bool { return strings.Contains(s, "://") }

// resolveInclude turns the text of an include into the file or URL to read,
// relative to the file that contains it. A file that was itself fetched from a
// URL can include only URLs (relative ones are resolved against its own URL), so
// a remote file can never make verifien read a local one.
func resolveInclude(from, path string) (string, error) {
	switch {
	case isURL(path):
		u, err := url.Parse(path)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return "", fmt.Errorf("%q is not a file name or an http:// or https:// URL", path)
		}
		return u.String(), nil
	case isURL(from):
		base, err := url.Parse(from)
		if err != nil {
			return "", err
		}
		ref, err := url.Parse(path)
		if err != nil {
			return "", fmt.Errorf("%q: %w", path, err)
		}
		return base.ResolveReference(ref).String(), nil
	case filepath.IsAbs(path):
		return filepath.Clean(path), nil
	default:
		return filepath.Join(filepath.Dir(from), path), nil
	}
}

// identity is how a source is compared when looking for include loops.
func identity(src string) string {
	if isURL(src) {
		return src
	}
	if abs, err := filepath.Abs(src); err == nil {
		return abs
	}
	return filepath.Clean(src)
}

// readSource returns the text of a file or URL. missing is true when it is not
// there (no such file, or HTTP 404/410), which is the only thing sinclude skips.
func readSource(src string) (body []byte, missing bool, err error) {
	if !isURL(src) {
		b, err := os.ReadFile(src)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, true, err
		}
		if err == nil && len(b) > includeMaxSize {
			err = fmt.Errorf("%s is larger than %d bytes", src, includeMaxSize)
		}
		return b, false, err
	}
	client := &http.Client{Timeout: includeTimeout}
	resp, err := client.Get(src)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return nil, true, fmt.Errorf("%s: %s", src, resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("%s: %s", src, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, includeMaxSize+1))
	if err == nil && len(b) > includeMaxSize {
		err = fmt.Errorf("%s is larger than %d bytes", src, includeMaxSize)
	}
	return b, false, err
}

// expand replaces every include in f (read from name) with the contents of the
// file it names, recursively, and returns one File:
//
//   - sections come out in order, an included file's sections where its include was;
//   - there is one [DEFAULTS] block. For a key given in more than one file, the
//     including file wins over what it includes, and a later include wins over
//     an earlier one;
//   - there is one [ACTION_SCRIPTS] block holding every script, so the same
//     as:IDENT defined in two files is an error.
//
// Each file may still have only one [DEFAULTS] and one [ACTION_SCRIPTS] of its own.
// stack holds the files being read right now, to catch include loops.
func expand(parser *participle.Parser[File], f *File, name string, stack []string) (*File, error) {
	var (
		own       *DefaultsSection
		ownScript *ScriptsSection
		defs      []*Option
		defsPos   *lexer.Position
		scripts   []*ScriptDef
		scrPos    *lexer.Position
		sections  []*Item
	)
	addDefaults := func(pos lexer.Position, opts []*Option) {
		if defsPos == nil {
			p := pos
			defsPos = &p
		}
		replaced := map[string]bool{}
		for _, o := range opts {
			replaced[canonicalKey(o.Key)] = true
		}
		kept := defs[:0:0]
		for _, o := range defs {
			if !replaced[canonicalKey(o.Key)] {
				kept = append(kept, o)
			}
		}
		defs = append(kept, opts...)
	}
	addScripts := func(pos lexer.Position, list []*ScriptDef) {
		if scrPos == nil {
			p := pos
			scrPos = &p
		}
		scripts = append(scripts, list...)
	}

	for _, it := range f.Items {
		switch {
		case it.Defaults != nil:
			if own != nil {
				return nil, fmt.Errorf("%s: [DEFAULTS] appears twice (first at %s)", it.Defaults.Pos, own.Pos)
			}
			own = it.Defaults
		case it.Scripts != nil:
			if ownScript != nil {
				return nil, fmt.Errorf("%s: [ACTION_SCRIPTS] appears twice (first at %s)", it.Scripts.Pos, ownScript.Pos)
			}
			ownScript = it.Scripts
		case it.Section != nil:
			sections = append(sections, it)
		case it.Include != nil:
			inc := it.Include
			target, err := resolveInclude(name, inc.Path)
			if err != nil {
				return nil, fmt.Errorf("%s: %s: %w", inc.Pos, inc.Kw, err)
			}
			id := identity(target)
			for _, s := range stack {
				if s == id {
					return nil, fmt.Errorf("%s: %s %q would include a file that is already being read (loop: %s -> %s)",
						inc.Pos, inc.Kw, inc.Path, strings.Join(stack, " -> "), id)
				}
			}
			body, missing, err := readSource(target)
			if err != nil {
				if missing && inc.optional() {
					continue
				}
				return nil, fmt.Errorf("%s: %s %q: %w", inc.Pos, inc.Kw, inc.Path, err)
			}
			child, err := parser.ParseString(target, string(body))
			if err != nil {
				return nil, err
			}
			child, err = expand(parser, child, target, append(append([]string(nil), stack...), id))
			if err != nil {
				return nil, err
			}
			for _, ci := range child.Items {
				switch {
				case ci.Defaults != nil:
					addDefaults(ci.Defaults.Pos, ci.Defaults.Options.Items)
				case ci.Scripts != nil:
					addScripts(ci.Scripts.Pos, ci.Scripts.Scripts)
				default:
					sections = append(sections, ci)
				}
			}
		}
	}
	// The including file's own blocks go last, so they win.
	if own != nil {
		addDefaults(own.Pos, own.Options.Items)
	}
	if ownScript != nil {
		addScripts(ownScript.Pos, ownScript.Scripts)
	}

	out := &File{}
	if defsPos != nil {
		hdr := "[DEFAULTS]"
		if own != nil {
			hdr = own.Header
		}
		out.Items = append(out.Items, &Item{Defaults: &DefaultsSection{Pos: *defsPos, Header: hdr, Options: &Options{Items: defs}}})
	}
	if scrPos != nil {
		out.Items = append(out.Items, &Item{Scripts: &ScriptsSection{Pos: *scrPos, Header: "[ACTION_SCRIPTS]", Scripts: scripts}})
	}
	out.Items = append(out.Items, sections...)
	return out, nil
}
