package fixup

import (
	"bytes"
	"net/http"
)

// XMLRewriter wraps an http.Handler and fixes the XML namespace prefix
// that govmomi's encoder generates. It replaces _XMLSchema-instance with xsi
// in SOAP responses so libvirt's ESX driver can parse them.
//
// govmomi's vim25/xml encoder generates:
//
//	xmlns:_XMLSchema-instance="http://www.w3.org/2001/XMLSchema-instance"
//	_XMLSchema-instance:type="xsd:string"
//
// but libvirt expects the standard xsi prefix:
//
//	xsi:type="xsd:string"
type XMLRewriter struct {
	Handler http.Handler
}

func (rw *XMLRewriter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := &responseRecorder{
		header: make(http.Header),
		body:   &bytes.Buffer{},
	}

	rw.Handler.ServeHTTP(rec, r)

	body := rec.body.Bytes()

	// Only rewrite XML/SOAP responses, not binary disk data
	ct := rec.header.Get("Content-Type")
	if len(body) > 0 && (ct == "" || ct == "text/xml" || ct == "text/xml; charset=utf-8" || ct == "application/xml") {
		body = bytes.ReplaceAll(body,
			[]byte(`xmlns:_XMLSchema-instance="http://www.w3.org/2001/XMLSchema-instance"`),
			[]byte{})
		body = bytes.ReplaceAll(body,
			[]byte(`_XMLSchema-instance:type=`),
			[]byte(`xsi:type=`))
		body = bytes.ReplaceAll(body,
			[]byte(`_XMLSchema-instance:nil=`),
			[]byte(`xsi:nil=`))
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
