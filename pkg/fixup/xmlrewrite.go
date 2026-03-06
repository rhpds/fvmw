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

// untypedMORRe matches ManagedObjectReference elements with type= but without xsi:type.
// Matches: <ManagedObjectReference type="Foo">
// Skips:   <ManagedObjectReference type="Foo" xsi:type="...">
var untypedMORRe = regexp.MustCompile(`<ManagedObjectReference (type="[^"]*")>`)

// untypedIntRe matches <int> elements without xsi:type (inside ArrayOfInt).
var untypedIntRe = regexp.MustCompile(`<int>`)

// untypedStringRe matches <string> elements without xsi:type (inside ArrayOfString).
var untypedStringRe = regexp.MustCompile(`<string>`)

// untypedBoolRe matches <boolean> elements without xsi:type.
var untypedBoolRe = regexp.MustCompile(`<boolean>`)

// untypedValueRe matches <value>...</value> elements without an xsi:type attribute.
// These occur when vcsim serializes Go interface{} values (e.g. OptionValue.Value).
var untypedValueRe = regexp.MustCompile(`<value>([^<]*)</value>`)

func addValueType(match []byte) []byte {
	// Extract the content
	content := untypedValueRe.FindSubmatch(match)
	if content == nil {
		return match
	}
	val := string(content[1])

	// Determine the XSD type from the value
	xsdType := "xsd:string"
	if val == "true" || val == "false" {
		xsdType = "xsd:boolean"
	} else if regexp.MustCompile(`^-?\d+$`).MatchString(val) {
		xsdType = "xsd:int"
	} else if regexp.MustCompile(`^-?\d+\.\d+$`).MatchString(val) {
		xsdType = "xsd:float"
	}

	return []byte(`<value xsi:type="` + xsdType + `">` + val + `</value>`)
}

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
		// Replace the declaration to use xsi prefix, and replace all usages.
		// Keep the declaration (don't strip it) so the namespace is always bound.
		body = bytes.ReplaceAll(body,
			[]byte(`xmlns:_XMLSchema-instance="http://www.w3.org/2001/XMLSchema-instance"`),
			[]byte(`xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"`))
		body = bytes.ReplaceAll(body,
			[]byte(`_XMLSchema-instance:type=`),
			[]byte(`xsi:type=`))
		body = bytes.ReplaceAll(body,
			[]byte(`_XMLSchema-instance:nil=`),
			[]byte(`xsi:nil=`))

		// Fix 2: strip empty changeSets (nil values that libvirt can't parse)
		body = emptyChangeSetRe.ReplaceAll(body, []byte{})

		// Fix 3: add xsi:type to untyped <value> elements inside OptionValue.
		// vcsim serializes Go interface{} values without xsi:type, but libvirt's
		// ESX driver deserializes them as AnyType which requires the type attribute.
		body = untypedValueRe.ReplaceAllFunc(body, addValueType)

		// Fix 4: add xsi:type to children of ArrayOf* elements.
		// libvirt's esxVI_AnyType_DeserializeList calls esxVI_AnyType_Deserialize
		// on each child of ArrayOf* values, which requires xsi:type.
		// vcsim omits xsi:type on ManagedObjectReference and int children.
		body = untypedMORRe.ReplaceAll(body,
			[]byte(`<ManagedObjectReference $1 xsi:type="ManagedObjectReference">`))
		body = untypedIntRe.ReplaceAll(body,
			[]byte(`<int xsi:type="xsd:int">`))
		body = untypedStringRe.ReplaceAll(body,
			[]byte(`<string xsi:type="xsd:string">`))
		body = untypedBoolRe.ReplaceAll(body,
			[]byte(`<boolean xsi:type="xsd:boolean">`))
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
