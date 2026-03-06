package fixup

import (
	"bytes"
	"net/http"
	"regexp"
)

// XMLRewriter wraps an http.Handler and fixes XML compatibility issues
// between govmomi's vcsim and libvirt's ESX driver (used by virt-v2v).
//
// Fixes applied:
//
//  1. Namespace prefix: govmomi generates _XMLSchema-instance: instead of xsi:
//     for the XML Schema Instance namespace. Libvirt's xmlGetNsProp resolves
//     by URI so this shouldn't matter, but we fix it for safety.
//
//  2. Empty changeSets: vcsim returns <changeSet> with op=assign but no <val>
//     for nil Go values (e.g. cpuHotAddEnabled=nil). Libvirt tries to
//     deserialize the missing value as AnyType and fails with
//     "AnyType is missing 'type' property". We strip these empty changeSets.
type XMLRewriter struct {
	Handler http.Handler
}

// emptyChangeSetRe matches changeSet elements that have op=assign but no <val>.
// These are nil values in vcsim that libvirt can't parse.
// Example: <changeSet><name>config.cpuHotAddEnabled</name><op>assign</op></changeSet>
var emptyChangeSetRe = regexp.MustCompile(
	`<changeSet><name>[^<]+</name><op>assign</op></changeSet>`,
)

func (rw *XMLRewriter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := &responseRecorder{
		header: make(http.Header),
		body:   &bytes.Buffer{},
		code:   http.StatusOK,
	}

	rw.Handler.ServeHTTP(rec, r)

	body := rec.body.Bytes()

	// Only rewrite XML/SOAP responses, not binary disk data
	ct := rec.header.Get("Content-Type")
	if len(body) > 0 && (ct == "" || ct == "text/xml" || ct == "text/xml; charset=utf-8" || ct == "application/xml") {
		// Fix 1: namespace prefix
		body = bytes.ReplaceAll(body,
			[]byte(`xmlns:_XMLSchema-instance="http://www.w3.org/2001/XMLSchema-instance"`),
			[]byte{})
		body = bytes.ReplaceAll(body,
			[]byte(`_XMLSchema-instance:type=`),
			[]byte(`xsi:type=`))
		body = bytes.ReplaceAll(body,
			[]byte(`_XMLSchema-instance:nil=`),
			[]byte(`xsi:nil=`))

		// Fix 2: strip empty changeSets (nil values that libvirt can't parse)
		body = emptyChangeSetRe.ReplaceAll(body, []byte{})
	}

	for k, vals := range rec.header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(rec.code)
	_, _ = w.Write(body)
}

type responseRecorder struct {
	header http.Header
	body   *bytes.Buffer
	code   int
}

func (r *responseRecorder) Header() http.Header {
	return r.header
}

func (r *responseRecorder) WriteHeader(code int) {
	r.code = code
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	return r.body.Write(b)
}
