// Copyright 2011 Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"exec"
	"flag"
	"fmt"
	"go/token"
	"go/parser"
	"go/printer"
	"http"
	"io/ioutil"
	"json"
	"os"
	"path/filepath"
	"log"
	"strings"
	"unicode"
	"url"
)

// goGenVersion is the version of the Go code generator
const goGenVersion = "0.5"

var (
	apiToGenerate = flag.String("api", "*", "The API ID to generate, like 'tasks:v1'. A value of '*' means all.")
	useCache      = flag.Bool("cache", true, "Use cache of discovered Google API discovery documents.")
	genDir        = flag.String("gendir", "", "Directory to use to write out generated Go files and Makefiles")
	build         = flag.Bool("build", false, "Compile generated packages.")
	install       = flag.Bool("install", false, "Install generated packages.")

	publicOnly = flag.Bool("publiconly", true, "Only build public, released APIs. Only applicable for Google employees.")
)

// API represents an API to generate, as well as its state while it's
// generating.
type API struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Version       string `json:"version"`
	Title         string `json:"title"`
	DiscoveryLink string `json:"discoveryLink"` // relative
	Preferred     bool   `json:"preferred"`

	m map[string]interface{}

	usedNames namePool
	schemas   map[string]*Schema // apiName -> schema

	p  func(format string, args ...interface{}) // print raw
	pn func(format string, args ...interface{}) // print with indent and newline
}

type AllAPIs struct {
	Items []*API `json:"items"`
}

type generateError struct {
	api   *API
	error os.Error
}

func (e *generateError) String() string {
	return fmt.Sprintf("API %s failed to generate code: %v", e.api.ID, e.error)
}

type compileError struct {
	api    *API
	output string
}

func (e *compileError) String() string {
	return fmt.Sprintf("API %s failed to compile:\n%v", e.api.ID, e.output)
}

func main() {
	flag.Parse()

	if *install {
		*build = true
	}

	var (
		apiIds  = []string{}
		matches = []*API{}
		errors  = []os.Error{}
	)
	for _, api := range getAPIs() {
		apiIds = append(apiIds, api.ID)
		if !api.want() {
			continue
		}
		matches = append(matches, api)
		log.Printf("Generating API %s", api.ID)
		err := api.GenerateCode()
		if err != nil {
			errors = append(errors, &generateError{api, err})
			continue
		}
		if *build {
			args := []string{"-C", api.SourceDir()}
			if *install {
				args = append(args, "install")
			}
			out, err := exec.Command("make", args...).CombinedOutput()
			if err != nil {
				errors = append(errors, &compileError{api, string(out)})
			}
		}
	}

	if len(matches) == 0 {
		log.Fatalf("No APIs matched %q; options are %v", *apiToGenerate, apiIds)
	}

	if len(errors) > 0 {
		log.Printf("%d API(s) failed to generate or compile:", len(errors))
		for _, ce := range errors {
			log.Printf(ce.String())
		}
		os.Exit(1)
	}
}

func (a *API) want() bool {
	return *apiToGenerate == "*" || *apiToGenerate == a.ID
}

func getAPIs() []*API {
	const apisURL = "https://www.googleapis.com/discovery/v1/apis"

	var all AllAPIs
	disco := slurpURL(apisURL)
	if err := json.Unmarshal(disco, &all); err != nil {
		log.Fatalf("error decoding JSON in %s: %v", apisURL, err)
	}
	return all.Items
}

func writeFile(file string, contents []byte) os.Error {
	// Don't write it if the contents are identical.
	existing, err := ioutil.ReadFile(file)
	if err == nil && bytes.Equal(existing, contents) {
		return nil
	}
	return ioutil.WriteFile(file, contents, 0644)
}

func slurpURL(urlStr string) []byte {
	diskFile := filepath.Join(os.TempDir(), "google-api-cache-"+url.QueryEscape(urlStr))
	if *useCache {
		bs, err := ioutil.ReadFile(diskFile)
		if err == nil && len(bs) > 0 {
			return bs
		}
	}

	req, err := http.NewRequest("GET", urlStr, nil)
	if err != nil {
		log.Fatal(err)
	}
	if *publicOnly {
		req.Header.Add("X-User-IP", "0.0.0.0") // hack
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatalf("Error fetching URL %s: %v", urlStr, err)
	}
	bs, err := ioutil.ReadAll(res.Body)
	if err != nil {
		log.Fatalf("Error reading body of URL %s: %v", urlStr, err)
	}
	if *useCache {
		if err := ioutil.WriteFile(diskFile, bs, 0666); err != nil {
			log.Printf("Warning: failed to write JSON of %s to disk file %s: %v", urlStr, diskFile, err)
		}
	}
	return bs
}

// namePool keeps track of used names and assigns free ones based on a
// preferred name
type namePool struct {
	m map[string]bool // lazily initialized
}

func (p *namePool) Get(preferred string) string {
	if p.m == nil {
		p.m = make(map[string]bool)
	}
	name := preferred
	tries := 0
	for p.m[name] {
		tries++
		name = fmt.Sprintf("%s%d", preferred, tries)
	}
	p.m[name] = true
	return name
}

func (a *API) SourceDir() string {
	return filepath.Join(*genDir, a.Package(), a.Version)
}

func (a *API) DiscoveryURL() string {
	if a.DiscoveryLink == "" {
		log.Fatalf("API %s has no DiscoveryLink", a.ID)
	}
	base, _ := url.Parse("https://www.googleapis.com/discovery/v1/apis")
	u, err := base.Parse(a.DiscoveryLink)
	if err != nil {
		log.Fatalf("API %s has bogus DiscoveryLink %s: %v", a.ID, a.DiscoveryLink, err)
	}
	return u.String()
}

func (a *API) Package() string {
	return strings.ToLower(a.Name)
}

func (a *API) Target() string {
	return fmt.Sprintf("google-api-go-client.googlecode.com/hg/%s/%s", a.Package(), a.Version)
}

// GetName returns a free top-level function/type identifier in the package.
// It tries to return your preferred match if it's free.
func (a *API) GetName(preferred string) string {
	return a.usedNames.Get(preferred)
}

func (a *API) apiBaseURL() string {
	return resolveRelative("https://www.googleapis.com/discovery/v1/apis", jstr(a.m, "basePath"))
}

func (a *API) needsDataWrapper() bool {
	for _, feature := range jstrlist(a.m, "features") {
		if feature == "dataWrapper" {
			return true
		}
	}
	return false
}

func (a *API) GenerateCode() (outerr os.Error) {
	a.m = make(map[string]interface{})
	m := a.m
	jsonBytes := slurpURL(a.DiscoveryURL())
	err := json.Unmarshal(jsonBytes, &a.m)
	if err != nil {
		return err
	}

	outdir := a.SourceDir()
	err = os.MkdirAll(outdir, 0755)
	if err != nil {
		return fmt.Errorf("failed to Mkdir %s: %v", outdir, err)
	}

	pkg := a.Package()
	makefilename := filepath.Join(outdir, "Makefile")
	makefile := "include $(GOROOT)/src/Make.inc\n" +
		"PREREQ=$(QUOTED_GOROOT)/pkg/$(GOOS)_$(GOARCH)/google-api-go-client.googlecode.com/hg/google-api.a\n" +
		"TARG=" + a.Target() + "\n" +
		"GOFILES=" + a.Package() + "-gen.go\n" +
		"include $(GOROOT)/src/Make.pkg\n"
	err = ioutil.WriteFile(makefilename, []byte(makefile), 0666)
	if err != nil {
		return fmt.Errorf("failed to write Makefile %s: %v", makefilename, err)
	}
	writeFile(filepath.Join(outdir, a.Package()+"-api.json"), jsonBytes)

	genfilename := filepath.Join(outdir, pkg+"-gen.go")

	// Buffer the output in memory, for gofmt'ing later in the defer.
	var buf bytes.Buffer
	a.p = func(format string, args ...interface{}) {
		_, err := fmt.Fprintf(&buf, format, args...)
		if err != nil {
			log.Fatalf("Error writing to %s: %v", genfilename, err)
		}
	}
	a.pn = func(format string, args ...interface{}) {
		a.p(format+"\n", args...)
	}

	// Write the file out after running gofmt on it.
	defer func() {
		if outerr != nil {
			writeFile(genfilename, buf.Bytes())
			return
		}

		fset := token.NewFileSet()
		ast, err := parser.ParseFile(fset, "", buf.Bytes(), parser.ParseComments)
		if err != nil {
			writeFile(genfilename, buf.Bytes())
			outerr = err
			return
		}

		var clean bytes.Buffer
		_, err = (&printer.Config{printer.TabIndent | printer.UseSpaces, 8}).Fprint(&clean, fset, ast)
		if err != nil {
			outerr = err
			writeFile(genfilename, buf.Bytes())
			return
		}
		if err := writeFile(genfilename, clean.Bytes()); err != nil {
			outerr = err
		}
	}()

	p, pn := a.p, a.pn

	reslist := a.Resources()

	p("// Package %s provides access to the %s.\n", pkg, jstr(m, "title"))
	if docs := jstr(m, "documentationLink"); docs != "" {
		p("//\n")
		p("// See %s\n", docs)
	}
	p("//\n// Usage example:\n")
	p("//\n")
	p("//   import %q\n", a.Target())
	p("//   ...\n")
	p("//   %sService, err := %s.New(oauthHttpClient)\n", pkg, pkg)

	p("package %s\n", pkg)
	p("\n")
	p("import (\n")
	for _, pkg := range []string{"bytes", "fmt", "http", "io", "json", "os", "strings", "strconv", "url",
		"google-api-go-client.googlecode.com/hg/google-api"} {
		p("\t%q\n", pkg)
	}
	p(")\n\n")
	pn("var _ = bytes.NewBuffer")
	pn("var _ = strconv.Itoa")
	pn("var _ = fmt.Sprintf")
	pn("var _ = json.NewDecoder")
	pn("var _ = io.Copy")
	pn("var _ = url.Parse")
	pn("var _ = googleapi.Version")
	pn("")
	pn("const apiId = %q", jstr(m, "id"))
	pn("const apiName = %q", jstr(m, "name"))
	pn("const apiVersion = %q", jstr(m, "version"))
	p("const basePath = %q\n", a.apiBaseURL())
	p("\n")

	a.generateScopeConstants()

	a.GetName("New") // ignore return value; we're the first caller
	pn("func New(client *http.Client) (*Service, os.Error) {")
	pn("if client == nil { return nil, os.NewError(\"client is nil\") }")
	pn("s := &Service{client: client}")
	for _, res := range reslist {
		pn("s.%s = &%s{s: s}", res.GoField(), res.GoType())
	}
	pn("return s, nil")
	pn("}")

	a.GetName("Service") // ignore return value; no user-defined names yet
	p("\ntype Service struct {\n")
	p("\tclient *http.Client\n")

	for _, res := range reslist {
		p("\n\t%s\t*%s\n", res.GoField(), res.GoType())
	}
	p("}\n")

	for _, res := range reslist {
		p("\ntype %s struct {\n", res.GoType())
		p("\ts *Service\n}\n")
	}

	a.PopulateSchemas()

	for _, s := range a.schemas {
		s.writeSchemaCode()
	}

	for _, meth := range a.APIMethods() {
		meth.generateCode()
	}

	for _, res := range reslist {
		for _, meth := range res.Methods() {
			meth.generateCode()
		}
	}

	pn("\nfunc cleanPathString(s string) string { return strings.Map(func(r int) int { if r >= 0x30 && r <= 0x7a { return r }; return -1 }, s) }")
	return nil
}

func (a *API) generateScopeConstants() {
	auth := jobj(a.m, "auth")
	if auth == nil {
		return
	}
	oauth2 := jobj(auth, "oauth2")
	if oauth2 == nil {
		return
	}
	scopes := jobj(oauth2, "scopes")
	if scopes == nil || len(scopes) == 0 {
		return
	}

	a.p("// OAuth2 scopes used by this API.\n")
	a.p("const (\n")
	n := 0
	for scope, mi := range scopes {
		if n > 0 {
			a.p("\n")
		}
		n++
		ident := scopeIdentifierFromURL(scope)
		if des := jstr(mi.(map[string]interface{}), "description"); des != "" {
			a.p("%s", asComment("\t", des))
		}
		a.p("\t%s = %q\n", ident, scope)
	}
	a.p(")\n\n")
}

func scopeIdentifierFromURL(urlStr string) string {
	const prefix = "https://www.googleapis.com/auth/"
	if !strings.HasPrefix(urlStr, prefix) {
		log.Fatalf("Unexpected oauth2 scope %q doesn't start with %q", urlStr, prefix)
	}
	ident := validGoIdentifer(initialCap(urlStr[len(prefix):])) + "Scope"
	return ident
}

type Schema struct {
	api *API
	m   map[string]interface{} // original JSON map

	typ *Type // lazily populated by Type

	apiName string // the native API-defined name of this type
	goName  string // lazily populated by GoName
}

type Property struct {
	s       *Schema                // property of which schema
	apiName string                 // the native API-defined name of this property
	m       map[string]interface{} // original JSON map

	typ *Type // lazily populated by Type
}

func (p *Property) Type() *Type {
	if p.typ == nil {
		p.typ = &Type{api: p.s.api, m: p.m}
	}
	return p.typ
}

func (p *Property) GoName() string {
	return initialCap(p.apiName)
}

func (p *Property) APIName() string {
	return p.apiName
}

func (p *Property) Description() string {
	return jstr(p.m, "description")
}

type Type struct {
	m   map[string]interface{} // JSON map containing key "type" and maybe "items", "properties"
	api *API
}

func (t *Type) apiType() string {
	// Note: returns "" on reference types
	if t, ok := t.m["type"].(string); ok {
		return t
	}
	return ""
}

func (t *Type) apiTypeFormat() string {
	if f, ok := t.m["format"].(string); ok {
		return f
	}
	return ""
}

func (t *Type) asSimpleGoType() (goType string, ok bool) {
	return simpleTypeConvert(t.apiType(), t.apiTypeFormat())
}

func (t *Type) String() string {
	return fmt.Sprintf("[type=%q, map=%s]", t.apiType(), prettyJSON(t.m))
}

func (t *Type) AsGo() string {
	if t, ok := t.asSimpleGoType(); ok {
		return t
	}
	if at, ok := t.ArrayType(); ok {
		return "[]" + at.AsGo()
	}
	if ref, ok := t.Reference(); ok {
		s := t.api.schemas[ref]
		if s == nil {
			panic(fmt.Sprintf("in Type.AsGo(), failed to find referenced type %q for %s",
				ref, prettyJSON(t.m)))
		}
		return s.Type().AsGo()
	}
	if t.IsStruct() {
		if apiName, ok := t.m["_apiName"].(string); ok {
			s := t.api.schemas[apiName]
			if s == nil {
				panic(fmt.Sprintf("in Type.AsGo, _apiName of %q didn't point to a valid schema; json: %s",
					apiName, prettyJSON(t.m)))
			}
			return "*" + s.GoName()
		}
		panic("in Type.AsGo, no _apiName found for struct type " + prettyJSON(t.m))
	}
	panic("unhandled Type.AsGo for " + prettyJSON(t.m))
}

func (t *Type) IsSimple() bool {
	_, ok := simpleTypeConvert(t.apiType(), t.apiTypeFormat())
	return ok
}

func (t *Type) IsStruct() bool {
	return t.apiType() == "object"
}

func (t *Type) Reference() (apiName string, ok bool) {
	apiName = jstr(t.m, "$ref")
	ok = apiName != ""
	return
}

func (t *Type) IsReference() bool {
	return jstr(t.m, "$ref") != ""
}

func (t *Type) ReferenceSchema() (s *Schema, ok bool) {
	apiName, ok := t.Reference()
	if !ok {
		return
	}

	s = t.api.schemas[apiName]
	if s == nil {
		log.Fatalf("failed to find t.api.schemas[%q] while resolving reference",
			apiName)
	}
	return s, true
}

func (t *Type) ArrayType() (elementType *Type, ok bool) {
	if t.apiType() != "array" {
		return
	}
	items := jobj(t.m, "items")
	if items == nil {
		log.Fatalf("can't handle array type missing its 'items' key. map is %#v", t.m)
	}
	return &Type{api: t.api, m: items}, true
}

func (s *Schema) Type() *Type {
	if s.typ == nil {
		s.typ = &Type{api: s.api, m: s.m}
	}
	return s.typ
}

func (s *Schema) properties() []*Property {
	if !s.Type().IsStruct() {
		panic("called properties on non-object schema")
	}
	pl := []*Property{}
	for name, im := range jobj(s.m, "properties") {
		m := im.(map[string]interface{})
		pl = append(pl, &Property{
			s:       s,
			m:       m,
			apiName: name,
		})
	}
	return pl
}

func (s *Schema) populateSubSchemas() {
	addSubStruct := func(subApiName string, t *Type) {
		if s.api.schemas[subApiName] != nil {
			panic("dup schema apiName: " + subApiName)
		}
		subm := t.m
		subm["_apiName"] = subApiName
		subs := &Schema{
			api:     s.api,
			m:       subm,
			typ:     t,
			apiName: subApiName,
		}
		s.api.schemas[subApiName] = subs
		subs.populateSubSchemas()
	}

	if s.Type().IsStruct() {
		for _, p := range s.properties() {
			if p.Type().IsSimple() {
				continue
			}
			if at, ok := p.Type().ArrayType(); ok {
				if at.IsSimple() || at.IsReference() {
					continue
				}
				subApiName := fmt.Sprintf("%s.%s", s.apiName, p.apiName)
				if at.IsStruct() {
					addSubStruct(subApiName, at) // was p.Type()?
					continue
				}
				if atat, ok := at.ArrayType(); ok {
					addSubStruct(subApiName, atat)
					continue
				}
				log.Fatalf("Unknown property array type for %q: %s", subApiName, at)
				continue
			}
			subApiName := fmt.Sprintf("%s.%s", s.apiName, p.apiName)
			if p.Type().IsStruct() {
				addSubStruct(subApiName, p.Type())
				continue
			}
			if p.Type().IsReference() {
				continue
			}
			log.Fatalf("Unknown type for %q: %s", subApiName, p.Type())
		}
		return
	}

	if at, ok := s.Type().ArrayType(); ok {
		if at.IsSimple() || at.IsReference() {
			return
		}
		subApiName := fmt.Sprintf("%s.Item", s.apiName)
		if at.IsStruct() {
			addSubStruct(subApiName, at)
			return
		}
		log.Fatalf("Unknown array type for %q: %s", subApiName, at)
		return
	}

	if s.Type().IsReference() {
		return
	}

	fmt.Fprintf(os.Stderr, "in populateSubSchemas, schema is: %s", prettyJSON(s.m))
	log.Fatalf("populateSubSchemas: unsupported type for schema %q", s.apiName)
}

// GoName returns (or creates and returns) the bare Go name
// of the apiName, making sure that it's a proper Go identifier
// and doesn't conflict with an existing name.
func (s *Schema) GoName() string {
	if s.goName == "" {
		s.goName = s.api.GetName(initialCap(s.apiName))
	}
	return s.goName
}

func (s *Schema) writeSchemaCode() {
	if s.Type().IsStruct() {
		s.writeSchemaStruct()
		return
	}

	if _, ok := s.Type().ArrayType(); ok {
		log.Printf("TODO writeSchemaCode for arrays for %s", s.GoName())
		return
	}

	if destSchema, ok := s.Type().ReferenceSchema(); ok {
		// Convert it to a struct using embedding.
		s.api.p("\ntype %s struct {\n", s.GoName())
		s.api.p("\t%s\n", destSchema.GoName())
		s.api.p("}\n")
		return
	}

	fmt.Fprintf(os.Stderr, "in writeSchemaCode, schema is: %s", prettyJSON(s.m))
	log.Fatalf("writeSchemaCode: unsupported type for schema %q", s.apiName)
}

func (s *Schema) writeSchemaStruct() {
	// TODO: description
	s.api.p("\ntype %s struct {\n", s.GoName())
	for i, p := range s.properties() {
		if i > 0 {
			s.api.p("\n")
		}
		pname := p.GoName()
		if des := p.Description(); des != "" {
			s.api.p("%s", asComment("\t", fmt.Sprintf("%s: %s", pname, des)))
		}
		s.api.p("\t%s %s `json:\"%s,omitempty\"`\n", pname, p.Type().AsGo(), p.APIName())
	}
	s.api.p("}\n")
}

// PopulateSchemas reads all the API types ("schemas") from the JSON file
// and converts them to *Schema instances, returning an identically
// keyed map, additionally containing subresources.  For instance,
//
// A resource "Foo" of type "object" with a property "bar", also of type
// "object" (an anonymous sub-resource), will get a synthetic API name
// of "Foo.bar".
//
// A resource "Foo" of type "array" with an "items" of type "object"
// will get a synthetic API name of "Foo.Item".
func (a *API) PopulateSchemas() {
	m := jobj(a.m, "schemas")
	if a.schemas != nil {
		panic("")
	}
	a.schemas = make(map[string]*Schema)
	for name, mi := range m {
		s := &Schema{
			api:     a,
			apiName: name,
			m:       mi.(map[string]interface{}),
		}

		// And a little gross hack, so a map alone is good
		// enough to get its apiName:
		s.m["_apiName"] = name

		a.schemas[name] = s
		s.populateSubSchemas()
	}
}

type Resource struct {
	api  *API
	name string
	m    map[string]interface{}
}

func (r *Resource) GoField() string {
	return initialCap(r.name)
}

func (r *Resource) GoType() string {
	return initialCap(r.name) + "Service"
}

func (r *Resource) Methods() []*Method {
	ms := []*Method{}
	for mname, mi := range jobj(r.m, "methods") {
		ms = append(ms, &Method{
			api:  r.api,
			r:    r,
			name: mname,
			m:    mi.(map[string]interface{}),
		})
	}
	return ms
}

type Method struct {
	api  *API
	r    *Resource // or nil if a API-level (top-level) method
	name string
	m    map[string]interface{} // original JSON

	params []*Param // all Params, of each type, lazily set by first access to Parameters
}

func (m *Method) Id() string {
	return jstr(m.m, "id")
}

func (m *Method) supportsMedia() bool {
	return jobj(m.m, "mediaUpload") != nil
}

func (m *Method) mediaPath() string {
	return jstr(jobj(jobj(jobj(m.m, "mediaUpload"), "protocols"), "simple"), "path")
}

func (m *Method) Params() []*Param {
	if m.params == nil {
		for name, mi := range jobj(m.m, "parameters") {
			pm := mi.(map[string]interface{})
			m.params = append(m.params, &Param{
				name:   name,
				m:      pm,
				method: m,
			})
		}
	}
	return m.params
}

func (m *Method) grepParams(f func(*Param) bool) []*Param {
	matches := make([]*Param, 0)
	for _, param := range m.Params() {
		if f(param) {
			matches = append(matches, param)
		}
	}
	return matches
}

func (m *Method) OptParams() []*Param {
	return m.grepParams(func(p *Param) bool {
		return !p.IsRequired()
	})
}

func (m *Method) RequiredRepeatedQueryParams() []*Param {
	return m.grepParams(func(p *Param) bool {
		return p.IsRequired() && p.IsRepeated() && p.Location() == "query"
	})
}

func (m *Method) RequiredQueryParams() []*Param {
	return m.grepParams(func(p *Param) bool {
		return p.IsRequired() && !p.IsRepeated() && p.Location() == "query"
	})
}

func (meth *Method) generateCode() {
	res := meth.r // may be nil if a top-level method
	a := meth.api
	p, pn := a.p, a.pn

	pn("\n// method id %q:", meth.Id())

	retTypeComma := responseType(meth.m)
	if retTypeComma != "" {
		retTypeComma += ", "
	}

	args := NewArguments(meth.m)
	methodName := initialCap(meth.name)

	prefix := ""
	if res != nil {
		prefix = initialCap(res.name)
	}
	callName := a.GetName(prefix + methodName + "Call")

	p("\ntype %s struct {\n", callName)
	p("\ts *Service\n")
	for _, arg := range args.l {
		p("\t%s %s\n", arg.goname, arg.gotype)
	}
	p("\topt_ map[string]interface{}\n")
	if meth.supportsMedia() {
		p("\tmedia_ io.Reader\n")
	}
	p("}\n")

	p("\n%s", asComment("", methodName+": "+jstr(meth.m, "description")))

	var servicePtr string
	if res == nil {
		p("func (s *Service) %s(%s) *%s {\n", methodName, args, callName)
		servicePtr = "s"
	} else {
		p("func (r *%s) %s(%s) *%s {\n", res.GoType(), methodName, args, callName)
		servicePtr = "r.s"
	}

	p("\tc := &%s{s: %s, opt_: make(map[string]interface{})}\n", callName, servicePtr)
	for _, arg := range args.l {
		p("\tc.%s = %s\n", arg.goname, arg.goname)
	}
	p("\treturn c\n")
	p("}\n")

	for _, opt := range meth.OptParams() {
		setter := initialCap(opt.name)
		des := jstr(opt.m, "description")
		des = strings.Replace(des, "Optional.", "", 1)
		des = strings.TrimSpace(des)
		p("\n%s", asComment("", fmt.Sprintf("%s sets the optional parameter %q: %s", setter, opt.name, des)))
		np := new(namePool)
		np.Get("c") // take the receiver's name
		paramName := np.Get(validGoIdentifer(opt.name))
		p("func (c *%s) %s(%s %s) *%s {\n", callName, setter, paramName, opt.GoType(), callName)
		p("c.opt_[%q] = %s\n", opt.name, paramName)
		p("return c\n")
		p("}\n")
	}

	if meth.supportsMedia() {
		p("func (c *%s) Media(r io.Reader) *%s {\n", callName, callName)
		p("c.media_ = r\n")
		p("return c\n")
		p("}\n")
	}

	pn("\nfunc (c *%s) Do() (%sos.Error) {", callName, retTypeComma)

	nilRet := ""
	if retTypeComma != "" {
		nilRet = "nil, "
	}
	pn("var body io.Reader = nil")
	hasContentType := false
	if ba := args.bodyArg(); ba != nil {
		style := "WithoutDataWrapper"
		if a.needsDataWrapper() {
			style = "WithDataWrapper"
		}
		pn("body, err := googleapi.%s.JSONReader(c.%s)", style, ba.goname)
		pn("if err != nil { return %serr }", nilRet)
		pn(`ctype := "application/json"`)
		hasContentType = true
	}
	pn("params := make(url.Values)")
	// Set this first. if they override it, though, might be gross.  We don't expect
	// XML replies elsewhere.  TODO(bradfitz): hide this option in the generated code?
	pn(`params.Set("alt", "json")`)
	for _, p := range meth.RequiredQueryParams() {
		pn("params.Set(%q, fmt.Sprintf(\"%%v\", c.%s))", p.name, p.name)
	}
	for _, p := range meth.RequiredRepeatedQueryParams() {
		pn("for _, v := range c.%s { params.Add(%q, fmt.Sprintf(\"%%v\", v)) }",
			p.name, p.name)
	}
	for _, p := range meth.OptParams() {
		pn("if v, ok := c.opt_[%q]; ok { params.Set(%q, fmt.Sprintf(\"%%v\", v)) }",
			p.name, p.name)
	}

	urlStr := resolveRelative(a.apiBaseURL(), jstr(meth.m, "path"))
	urlStr = strings.Replace(urlStr, "%7B", "{", -1)
	urlStr = strings.Replace(urlStr, "%7D", "}", -1)
	p("urls := googleapi.ResolveRelative(%q, %q)\n", a.apiBaseURL(), jstr(meth.m, "path"))
	if meth.supportsMedia() {
		pn("if c.media_ != nil {")
		// Hack guess, since we get a 404 otherwise:
		//pn("urls = googleapi.ResolveRelative(%q, %q)", a.apiBaseURL(), meth.mediaPath())
		// Further hack.  Discovery doc is wrong?
		pn("urls = strings.Replace(urls, %q, %q, 1)", "https://www.googleapis.com/", "https://www.googleapis.com/upload/")
		pn("}")
	}
	for _, arg := range args.forLocation("path") {
		p("\turls = strings.Replace(urls, \"{%s}\", %s, 1)\n", arg.apiname, arg.cleanExpr("c."))
	}
	pn("urls += \"?\" + params.Encode()")
	if meth.supportsMedia() {
		pn("contentLength_, hasMedia_ := googleapi.ConditionallyIncludeMedia(c.media_, &body, &ctype)")
	}
	pn("req, _ := http.NewRequest(%q, urls, body)", jstr(meth.m, "httpMethod"))
	if meth.supportsMedia() {
		pn("if hasMedia_ { req.ContentLength = contentLength_ }")
	}
	if hasContentType {
		pn(`req.Header.Set("Content-Type", ctype)`)
	}
	pn(`req.Header.Set("User-Agent", "google-api-go-client/` + goGenVersion + `")`)
	pn("res, err := c.s.client.Do(req);")
	pn("if err != nil { return %serr }", nilRet)
	pn("if err := googleapi.CheckResponse(res); err != nil { return %serr }", nilRet)
	if retTypeComma == "" {
		pn("return nil")
	} else {
		pn("ret := new(%s)", responseType(meth.m)[1:])
		pn("if err := json.NewDecoder(res.Body).Decode(ret); err != nil { return nil, err }")
		pn("return ret, nil")
	}

	bs, _ := json.MarshalIndent(meth.m, "\t// ", "  ")
	pn("// %s\n", string(bs))
	pn("}")
}

type Param struct {
	method *Method
	name   string
	m      map[string]interface{}
}

func (p *Param) IsRequired() bool {
	v, _ := p.m["required"].(bool)
	return v
}

func (p *Param) IsRepeated() bool {
	v, _ := p.m["repeated"].(bool)
	return v
}

func (p *Param) Location() string {
	return p.m["location"].(string)
}

func (p *Param) GoType() string {
	typ, format := jstr(p.m, "type"), jstr(p.m, "format")
	t, ok := simpleTypeConvert(typ, format)
	if !ok {
		panic("failed to convert parameter type " + fmt.Sprintf("type=%q, format=%q", typ, format))
	}
	return t
}

// APIMethods returns top-level ("API-level") methods. They don't have an associated resource.
func (a *API) APIMethods() []*Method {
	meths := []*Method{}
	for name, mi := range jobj(a.m, "methods") {
		meths = append(meths, &Method{
			api:  a,
			r:    nil, // to be explicit
			name: name,
			m:    mi.(map[string]interface{}),
		})
	}
	return meths
}

func (a *API) Resources() []*Resource {
	res := []*Resource{}
	for rname, rmi := range jobj(a.m, "resources") {
		rm := rmi.(map[string]interface{})
		res = append(res, &Resource{a, rname, rm})
	}
	return res
}

func resolveRelative(basestr, relstr string) string {
	u, _ := url.Parse(basestr)
	rel, _ := url.Parse(relstr)
	u = u.ResolveReference(rel)
	return u.String()
}

func NewArguments(m map[string]interface{}) (args *arguments) {
	args = &arguments{
		m: make(map[string]*argument),
	}
	po, ok := m["parameterOrder"].([]interface{})
	if ok {
		for _, poi := range po {
			pname := poi.(string)
			arg := NewArg(pname, jobj(jobj(m, "parameters"), pname))
			args.AddArg(arg)
		}
	}
	if ro := jobj(m, "request"); ro != nil {
		arg := NewArg("REQUEST", ro)
		args.AddArg(arg)
	}
	return
}

func NewArg(apiname string, m map[string]interface{}) *argument {
	if apiname == "REQUEST" {
		reftype := jstr(m, "$ref")
		return &argument{
			goname:   validGoIdentifer(strings.ToLower(reftype)),
			apiname:  apiname,
			gotype:   "*" + reftype,
			apitype:  reftype,
			location: "body",
		}
	}
	repeated, _ := m["repeated"].(bool)
	apitype := jstr(m, "type")
	des := jstr(m, "description")
	goname := validGoIdentifer(apiname) // but might be changed later, if conflicts
	if strings.Contains(des, "identifier") {
		goname += "id" // yay
	}
	gotype := mustSimpleTypeConvert(apitype, jstr(m, "format"))
	if repeated {
		gotype = "[]" + gotype
	}
	return &argument{
		apiname:  apiname,
		apitype:  apitype,
		goname:   goname,
		gotype:   gotype,
		location: jstr(m, "location"),
	}
}

type argument struct {
	apiname, apitype string
	goname, gotype   string
	location         string // "path", "query", "body"
}

func (a *argument) String() string {
	return a.goname + " " + a.gotype
}

func (a *argument) cleanExpr(prefix string) string {
	switch a.gotype {
	case "string":
		return "cleanPathString(" + prefix + a.goname + ")"
	case "integer", "int64":
		return "strconv.Itoa64(" + prefix + a.goname + ")"
	}
	panic("unknown type: " + a.apitype)
}

// arguments are the arguments that a method takes
type arguments struct {
	l []*argument
	m map[string]*argument
}

func (args *arguments) forLocation(loc string) []*argument {
	matches := make([]*argument, 0)
	for _, arg := range args.l {
		if arg.location == loc {
			matches = append(matches, arg)
		}
	}
	return matches
}

func (args *arguments) bodyArg() *argument {
	for _, arg := range args.l {
		if arg.location == "body" {
			return arg
		}
	}
	return nil
}

func (args *arguments) AddArg(arg *argument) {
	n := 1
	oname := arg.goname
	for {
		_, present := args.m[arg.goname]
		if !present {
			args.m[arg.goname] = arg
			args.l = append(args.l, arg)
			return
		}
		n++
		arg.goname = fmt.Sprintf("%s%d", oname, n)
	}
}

func (a *arguments) String() string {
	var buf bytes.Buffer
	for i, arg := range a.l {
		if i != 0 {
			buf.Write([]byte(", "))
		}
		buf.Write([]byte(arg.String()))
	}
	return buf.String()
}

func asComment(pfx, c string) string {
	var buf bytes.Buffer
	const maxLen = 70
	removeNewlines := func(s string) string {
		return strings.Replace(s, "\n", "\n"+pfx+"// ", -1)
	}
	for len(c) > 0 {
		line := c
		if len(line) < maxLen {
			fmt.Fprintf(&buf, "%s// %s\n", pfx, removeNewlines(line))
			break
		}
		line = line[:maxLen]
		si := strings.LastIndex(line, " ")
		if si != -1 {
			line = line[:si]
		}
		fmt.Fprintf(&buf, "%s// %s\n", pfx, removeNewlines(line))
		c = c[len(line):]
		if si != -1 {
			c = c[1:]
		}
	}
	return buf.String()
}

func simpleTypeConvert(apiType, format string) (gotype string, ok bool) {
	// From http://tools.ietf.org/html/draft-zyp-json-schema-03#section-5.1
	switch apiType {
	case "boolean":
		gotype = "bool"
	case "string":
		gotype = "string"
		switch format {
		case "int64", "uint64", "int32", "uint32":
			gotype = format
		}
	case "number":
		gotype = "float64"
	case "integer":
		gotype = "int64"
	case "any":
		gotype = "interface{}"
	}
	return gotype, gotype != ""
}

func mustSimpleTypeConvert(apiType, format string) string {
	if gotype, ok := simpleTypeConvert(apiType, format); ok {
		return gotype
	}
	panic(fmt.Sprintf("failed to simpleTypeConvert(%q, %q)", apiType, format))
}

func (a *API) goTypeOfJsonObject(outerName, memberName string, m map[string]interface{}) (string, os.Error) {
	apitype := jstr(m, "type")
	switch apitype {
	case "array":
		items := jobj(m, "items")
		if items == nil {
			return "", os.NewError("no items but type was array")
		}
		if ref := jstr(items, "$ref"); ref != "" {
			return "[]*" + ref, nil // TODO: wrong; delete this whole function
		}
		if atype := jstr(items, "type"); atype != "" {
			return "[]" + mustSimpleTypeConvert(atype, jstr(items, "format")), nil
		}
		return "", os.NewError("unsupported 'array' type")
	case "object":
		return "*" + outerName + "_" + memberName, nil
		//return "", os.NewError("unsupported 'object' type")
	}
	return mustSimpleTypeConvert(apitype, jstr(m, "format")), nil
}

func responseType(m map[string]interface{}) string {
	ro := jobj(m, "response")
	if ro != nil {
		if ref := jstr(ro, "$ref"); ref != "" {
			return "*" + ref
		}
	}
	return ""
}

// initialCap returns the identifier with a leading capital letter.
// it also maps "foo-bar" to "FooBar".
func initialCap(ident string) string {
	if ident == "" {
		panic("blank identifier")
	}
	return depunct(ident, true)
}

func validGoIdentifer(ident string) string {
	id := depunct(ident, false)
	switch id {
	case "type":
		return "type_"
	}
	return id
}

// depunct removes '-', '.', '$', '/' from identifers, making the
// following character uppercase
func depunct(ident string, needCap bool) string {
	var buf bytes.Buffer
	for _, c := range ident {
		if c == '-' || c == '.' || c == '$' || c == '/' {
			needCap = true
			continue
		}
		if needCap {
			c = unicode.ToUpper(c)
			needCap = false
		}
		buf.WriteByte(byte(c))
	}
	return buf.String()

}

func prettyJSON(m map[string]interface{}) string {
	bs, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Sprintf("[JSON error %v on %#v]", err, m)
	}
	return string(bs)
}

func jstr(m map[string]interface{}, key string) string {
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

func jobj(m map[string]interface{}, key string) map[string]interface{} {
	if m, ok := m[key].(map[string]interface{}); ok {
		return m
	}
	return nil
}

func jstrlist(m map[string]interface{}, key string) []string {
	si, ok := m[key].([]interface{})
	if !ok {
		return nil
	}
	sl := make([]string, 0)
	for _, si := range si {
		sl = append(sl, si.(string))
	}
	return sl
}