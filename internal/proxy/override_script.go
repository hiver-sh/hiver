package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// scriptTimeout bounds a single override script's execution so a runaway script
// (e.g. an infinite loop) can't stall the request or pin a CPU.
const scriptTimeout = 200 * time.Millisecond

// maxScriptBody caps how much request body is read into memory for a script.
const maxScriptBody = 8 << 20 // 8 MiB

// runOverrideScript executes a per-rule Lua script against an inspected HTTP
// request, letting it rewrite the request body and headers programmatically —
// the escape hatch for cases the declarative EgressOverride can't express (e.g.
// decode a form/JSON body, substitute a substring, re-encode).
//
// The script runs in a restricted VM: only the base/string/table/math libraries
// are open (no os, io, package/require, and dofile/loadfile/load are removed),
// and execution is bounded by scriptTimeout. It sees these globals:
//
//	body     – request body as a string (rewrite it to change the body)
//	headers  – table of header name -> value (rewrite/add/remove to change them)
//	method   – request method (read-only)
//	host     – request host (read-only)
//	path     – request path (read-only)
//	query    – raw URL query, no leading "?" (read-only)
//
// and these helpers: urldecode, urlencode, b64decode, b64encode.
//
// After the script returns, the (possibly reassigned) body and headers globals
// are applied to the request. On any error the original request is left intact.
func runOverrideScript(r *http.Request, script string) error {
	var bodyBytes []byte
	if r.Body != nil {
		b, err := io.ReadAll(io.LimitReader(r.Body, maxScriptBody))
		_ = r.Body.Close()
		if err != nil {
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			return fmt.Errorf("read body: %w", err)
		}
		bodyBytes = b
	}

	L := lua.NewState(lua.Options{SkipOpenLibs: true})
	defer L.Close()

	ctx, cancel := context.WithTimeout(context.Background(), scriptTimeout)
	defer cancel()
	L.SetContext(ctx)

	openSafeLibs(L)
	registerHelpers(L)

	L.SetGlobal("body", lua.LString(string(bodyBytes)))
	L.SetGlobal("method", lua.LString(r.Method))
	L.SetGlobal("host", lua.LString(r.Host))
	if r.URL != nil {
		L.SetGlobal("path", lua.LString(r.URL.Path))
		L.SetGlobal("query", lua.LString(r.URL.RawQuery))
	}

	origHeaders := make(map[string]bool, len(r.Header))
	ht := L.NewTable()
	for k, vs := range r.Header {
		origHeaders[k] = true
		if len(vs) > 0 {
			ht.RawSetString(k, lua.LString(vs[0]))
		}
	}
	L.SetGlobal("headers", ht)

	if err := L.DoString(script); err != nil {
		// Leave the request unchanged on failure — restore the consumed body.
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		return err
	}

	applyScriptBody(r, bodyBytes, L)
	applyScriptHeaders(r, origHeaders, L)
	return nil
}

// openSafeLibs opens only the sandbox-safe standard libraries and strips the
// file-loading base functions.
func openSafeLibs(L *lua.LState) {
	for _, lib := range []struct {
		name string
		open lua.LGFunction
	}{
		{lua.BaseLibName, lua.OpenBase},
		{lua.StringLibName, lua.OpenString},
		{lua.TabLibName, lua.OpenTable},
		{lua.MathLibName, lua.OpenMath},
	} {
		L.Push(L.NewFunction(lib.open))
		L.Push(lua.LString(lib.name))
		L.Call(1, 0)
	}
	for _, name := range []string{"dofile", "loadfile", "load", "loadstring", "collectgarbage"} {
		L.SetGlobal(name, lua.LNil)
	}
}

// registerHelpers exposes the encode/decode helpers a rewrite typically needs.
func registerHelpers(L *lua.LState) {
	str1 := func(fn func(string) string) lua.LGFunction {
		return func(L *lua.LState) int {
			L.Push(lua.LString(fn(L.CheckString(1))))
			return 1
		}
	}
	L.SetGlobal("urldecode", L.NewFunction(str1(func(s string) string {
		if out, err := url.QueryUnescape(s); err == nil {
			return out
		}
		return s
	})))
	L.SetGlobal("urlencode", L.NewFunction(str1(url.QueryEscape)))
	L.SetGlobal("b64decode", L.NewFunction(str1(func(s string) string {
		if out, err := base64.StdEncoding.DecodeString(s); err == nil {
			return string(out)
		}
		return s
	})))
	L.SetGlobal("b64encode", L.NewFunction(str1(func(s string) string {
		return base64.StdEncoding.EncodeToString([]byte(s))
	})))
}

// applyScriptBody writes the script's (possibly rewritten) body back onto the
// request, keeping Content-Length in sync.
func applyScriptBody(r *http.Request, orig []byte, L *lua.LState) {
	newBody := orig
	if lv := L.GetGlobal("body"); lv.Type() == lua.LTString {
		newBody = []byte(lv.String())
	}
	r.Body = io.NopCloser(bytes.NewReader(newBody))
	r.ContentLength = int64(len(newBody))
	r.Header.Set("Content-Length", strconv.Itoa(len(newBody)))
}

// applyScriptHeaders reconciles r.Header with the script's headers table: string
// entries are set (added/overwritten), and any original header the script
// removed from the table is deleted.
func applyScriptHeaders(r *http.Request, orig map[string]bool, L *lua.LState) {
	lv := L.GetGlobal("headers")
	tbl, ok := lv.(*lua.LTable)
	if !ok {
		return
	}
	present := make(map[string]bool)
	tbl.ForEach(func(k, v lua.LValue) {
		if k.Type() != lua.LTString {
			return
		}
		name := k.String()
		present[textproto.CanonicalMIMEHeaderKey(name)] = true
		if v.Type() == lua.LTString {
			r.Header.Set(name, v.String())
		}
	})
	for name := range orig {
		if !present[name] {
			r.Header.Del(name)
		}
	}
}
