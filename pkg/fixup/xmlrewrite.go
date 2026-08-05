package fixup

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
)

// XMLRewriter wraps an http.Handler and fixes XML compatibility issues
// between govmomi's vcsim and libvirt's ESX driver (used by virt-v2v).
//
// Fixes applied:
//
//  1. Namespace prefix: govmomi generates _XMLSchema-instance: instead of xsi:
//  2. Empty changeSets: strip nil-value changeSets that libvirt can't parse
//  3. Untyped OptionValue: add xsi:type to <value> elements
//  4. ArrayOf* children: add xsi:type to MOR, int, string, boolean children
//  5. Optional HostAgent mode: rewrite apiType to HostAgent for matching hosts
//  6. Remove TenantTenantManager field for older VDDK compatibility
type XMLRewriter struct {
	Handler        http.Handler
	ESXiPrefix     string // optional hostname prefix that triggers HostAgent mode
	ScopeVMsByUser bool
	AllowedVMRefs  func(user string) map[string]struct{}

	mu           sync.Mutex
	soapSessions map[string]string // vmware_soap_session -> username
}

// emptyChangeSetRe matches changeSet elements that have op=assign but no <val>.
// These are nil values in vcsim that libvirt can't parse.
// Example: <changeSet><name>config.cpuHotAddEnabled</name><op>assign</op></changeSet>
var emptyChangeSetRe = regexp.MustCompile(
	`<changeSet><name>[^<]+</name><op>assign</op></changeSet>`,
)

// arrayOfRe finds ArrayOf* type declarations and their content.
// It captures the element type name from the ArrayOf prefix.
var arrayOfRe = regexp.MustCompile(`xsi:type="ArrayOf(\w+)"`)

// xsdPrimitives maps XML element names to their XSD types for ArrayOf children.
var xsdPrimitives = map[string]string{
	"int":     "xsd:int",
	"long":    "xsd:long",
	"short":   "xsd:short",
	"string":  "xsd:string",
	"boolean": "xsd:boolean",
	"float":   "xsd:float",
	"double":  "xsd:double",
	"byte":    "xsd:byte",
}

// addArrayOfChildTypes finds ArrayOf* elements and adds xsi:type to their children.
// For ArrayOfFoo, each <Foo> child gets xsi:type="Foo".
// For ArrayOfManagedObjectReference, each <ManagedObjectReference> gets xsi:type="ManagedObjectReference".
func addArrayOfChildTypes(body []byte) []byte {
	// Find all ArrayOf* type names used in the response
	matches := arrayOfRe.FindAllSubmatch(body, -1)
	seen := make(map[string]bool)
	for _, m := range matches {
		childType := string(m[1])
		if seen[childType] {
			continue
		}
		seen[childType] = true

		xsiType := childType
		if t, ok := xsdPrimitives[childType]; ok {
			// Primitive: <int> → xsi:type="xsd:int"
			xsiType = t
		}

		// For ManagedObjectReference, it has a type= attribute already
		if childType == "ManagedObjectReference" {
			// <ManagedObjectReference type="Foo"> → <ManagedObjectReference type="Foo" xsi:type="ManagedObjectReference">
			old := []byte(`<ManagedObjectReference type=`)
			new := []byte(`<ManagedObjectReference xsi:type="ManagedObjectReference" type=`)
			// Only replace ones that don't already have xsi:type
			if !bytes.Contains(body, []byte(`<ManagedObjectReference xsi:type=`)) {
				body = bytes.ReplaceAll(body, old, new)
			}
			continue
		}

		// For other types: <Foo> → <Foo xsi:type="Foo">
		// Only add if the element doesn't already have xsi:type
		old := []byte("<" + childType + ">")
		new := []byte("<" + childType + ` xsi:type="` + xsiType + `">`)
		body = bytes.ReplaceAll(body, old, new)
	}
	return body
}

// untypedValueRe matches <value>...</value> elements without an xsi:type attribute.
// These occur when vcsim serializes Go interface{} values (e.g. OptionValue.Value).
var untypedValueRe = regexp.MustCompile(`<value>([^<]*)</value>`)

var (
	soapLoginUserRe     = regexp.MustCompile(`(?s)<userName>([^<]+)</userName>`)
	soapSessionCookieRe = regexp.MustCompile(`(?:^|;\s*)vmware_soap_session=([^;]+)`)
	// soapVMObjectSetRe matches a PropertyCollector update entry for a single
	// VirtualMachine, as emitted by WaitForUpdatesEx — the actual
	// inventory-enumeration mechanism govmomi/forklift use, e.g.:
	//   <objectSet><kind>enter</kind><obj type="VirtualMachine">vm-23</obj><changeSet>...</changeSet></objectSet>
	soapVMObjectSetRe = regexp.MustCompile(`(?s)<objectSet><kind>\w+</kind><obj type="VirtualMachine">([^<]+)</obj>.*?</objectSet>`)
	// soapVMObjectContentRe matches the equivalent single-object entry as
	// emitted by RetrievePropertiesEx (a structurally different, non-nested
	// sibling-repeated format), e.g.:
	//   <objects><obj type="VirtualMachine">vm-23</obj><propSet>...</propSet></objects>
	soapVMObjectContentRe = regexp.MustCompile(`(?s)<objects><obj type="VirtualMachine">([^<]+)</obj>.*?</objects>`)
	soapVMMORInArrayRe   = regexp.MustCompile(`<ManagedObjectReference[^>]*type="VirtualMachine"[^>]*>([^<]+)</ManagedObjectReference>`)
	tenantManagerFieldRe = regexp.MustCompile(`(?s)<tenantManager[^>]*type="TenantTenantManager"[^>]*>.*?</tenantManager>`)
)

// emptyEnumDefaults maps XML element names for enum-typed fields (in
// govmomi's vim25 types) that vcsim never sets to a "not applicable"
// default value. These fields lack "omitempty" in their struct tags, so
// vcsim always emits them, but leaves them as the Go zero value (empty
// string) since it doesn't simulate the underlying feature.
var emptyEnumDefaults = map[string]string{
	"faultToleranceState":  "notConfigured",
	"guestHeartbeatStatus": "gray",
	"recordReplayState":    "inactive",
	"overallStatus":        "green",
}

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
	// Skip rewriting for binary/file paths — they'd OOM the buffer
	if strings.HasPrefix(r.URL.Path, "/folder/") || strings.HasPrefix(r.URL.Path, "/disk/") {
		rw.Handler.ServeHTTP(w, r)
		return
	}

	scopeSOAP := rw.shouldScopeSOAPRequest(r)

	var soapLoginUser string
	if scopeSOAP {
		reqBody, err := io.ReadAll(r.Body)
		if err == nil {
			r.Body = io.NopCloser(bytes.NewReader(reqBody))
			soapLoginUser = parseSOAPLoginUser(reqBody)
		}
	}

	rec := &responseRecorder{
		header: make(http.Header),
		body:   &bytes.Buffer{},
		code:   http.StatusOK,
	}

	rw.Handler.ServeHTTP(rec, r)

	body := rec.body.Bytes()

	if scopeSOAP {
		if soapLoginUser != "" {
			if id := parseSOAPSessionFromSetCookie(rec.header.Values("Set-Cookie")); id != "" {
				rw.mu.Lock()
				if rw.soapSessions == nil {
					rw.soapSessions = make(map[string]string)
				}
				rw.soapSessions[id] = soapLoginUser
				rw.mu.Unlock()
			}
		}
		if bytes.Contains(body, []byte("<LogoutResponse")) {
			if c, err := r.Cookie("vmware_soap_session"); err == nil {
				rw.mu.Lock()
				delete(rw.soapSessions, c.Value)
				rw.mu.Unlock()
			}
		}
		if user := rw.soapUserFromRequest(r); user != "" && rw.AllowedVMRefs != nil {
			body = filterSOAPVMsByAllowedRefs(body, rw.AllowedVMRefs(user))
		}
	}

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
		// vcsim omits xsi:type on ALL children inside ArrayOf* containers.
		body = addArrayOfChildTypes(body)

		// Fix 6: remove tenantTenantManager field.
		// Older VDDK/libvirt stacks can fail hard when encountering
		// the newer TenantTenantManager MOR type in ServiceContent.
		body = tenantManagerFieldRe.ReplaceAll(body, []byte{})

		// Fix 7: fill in empty enum fields that vcsim doesn't set a value
		// for (Go zero value = ""), but whose struct tags lack "omitempty"
		// so they're always emitted as an empty element. VDDK's strict
		// C++ SOAP deserializer rejects an empty string for enum-typed
		// fields (unlike govmomi's own lenient Go client), which breaks
		// its GetSummary call during VixDiskLib_Open. Fill with a valid
		// "not applicable" default for each.
		for elem, defaultVal := range emptyEnumDefaults {
			body = bytes.ReplaceAll(body,
				[]byte("<"+elem+"></"+elem+">"),
				[]byte("<"+elem+">"+defaultVal+"</"+elem+">"))
		}

		// Fix 5: Optional HostAgent mode — when the request host has the
		// configured prefix, rewrite apiType from VirtualCenter to HostAgent.
		if rw.ESXiPrefix != "" && strings.HasPrefix(r.Host, rw.ESXiPrefix) {
			log.Println("ESXi mode: rewriting apiType to HostAgent")
			body = bytes.ReplaceAll(body,
				[]byte(`<apiType>VirtualCenter</apiType>`),
				[]byte(`<apiType>HostAgent</apiType>`))
		}
	}

	for k, vals := range rec.header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}

	if r.URL.Path == "/sdk" {
		log.Printf("sdk request method=%s ua=%q status=%d response_len=%d", r.Method, r.UserAgent(), rec.code, len(body))
	}

	w.WriteHeader(rec.code)
	_, _ = w.Write(body)
}

func parseSOAPLoginUser(body []byte) string {
	if !bytes.Contains(body, []byte("<Login")) {
		return ""
	}
	match := soapLoginUserRe.FindSubmatch(body)
	if len(match) < 2 {
		return ""
	}
	return string(match[1])
}

func parseSOAPSessionFromSetCookie(setCookies []string) string {
	for _, line := range setCookies {
		if m := soapSessionCookieRe.FindStringSubmatch(line); len(m) >= 2 {
			return m[1]
		}
	}
	return ""
}

func filterSOAPVMsByAllowedRefs(body []byte, allowed map[string]struct{}) []byte {
	// Remove PropertyCollector object updates for VMs the user should not see
	// (WaitForUpdatesEx format).
	body = soapVMObjectSetRe.ReplaceAllFunc(body, func(block []byte) []byte {
		match := soapVMObjectSetRe.FindSubmatch(block)
		if len(match) < 2 {
			return block
		}
		if _, ok := allowed[string(match[1])]; ok {
			return block
		}
		return []byte{}
	})

	// Same, for the RetrievePropertiesEx format.
	body = soapVMObjectContentRe.ReplaceAllFunc(body, func(block []byte) []byte {
		match := soapVMObjectContentRe.FindSubmatch(block)
		if len(match) < 2 {
			return block
		}
		if _, ok := allowed[string(match[1])]; ok {
			return block
		}
		return []byte{}
	})

	// Remove VM ManagedObjectReference entries in arrays (e.g. childEntity listings).
	body = soapVMMORInArrayRe.ReplaceAllFunc(body, func(elem []byte) []byte {
		match := soapVMMORInArrayRe.FindSubmatch(elem)
		if len(match) < 2 {
			return elem
		}
		if _, ok := allowed[string(match[1])]; ok {
			return elem
		}
		return []byte{}
	})

	return body
}

func (rw *XMLRewriter) soapUserFromRequest(r *http.Request) string {
	cookie, err := r.Cookie("vmware_soap_session")
	if err != nil || cookie.Value == "" {
		return ""
	}
	rw.mu.Lock()
	defer rw.mu.Unlock()
	return rw.soapSessions[cookie.Value]
}

func (rw *XMLRewriter) shouldScopeSOAPRequest(r *http.Request) bool {
	if !rw.ScopeVMsByUser || r.Method != http.MethodPost || r.URL.Path != "/sdk" {
		return false
	}

	// Apply SOAP inventory scoping only to govmomi/forklift clients.
	// VDDK uses a different SOAP stack and has shown fragility with middleware
	// that inspects/mutates generic SOAP traffic.
	ua := strings.ToLower(r.UserAgent())
	return strings.Contains(ua, "forklift") || strings.Contains(ua, "govmomi")
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
