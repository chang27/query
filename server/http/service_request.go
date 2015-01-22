//  Copyright (c) 2014 Couchbase, Inc.
//  Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
//  except in compliance with the License. You may obtain a copy of the License at
//    http://www.apache.org/licenses/LICENSE-2.0
//  Unless required by applicable law or agreed to in writing, software distributed under the
//  License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
//  either express or implied. See the License for the specific language governing permissions
//  and limitations under the License.

package http

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/couchbaselabs/query/datastore"
	"github.com/couchbaselabs/query/errors"
	"github.com/couchbaselabs/query/plan"
	"github.com/couchbaselabs/query/server"
	"github.com/couchbaselabs/query/timestamp"
	"github.com/couchbaselabs/query/value"
)

const MAX_REQUEST_BYTES = 1 << 20

type httpRequest struct {
	server.BaseRequest
	resp         http.ResponseWriter
	req          *http.Request
	writer       responseDataManager
	httpRespCode int
	resultCount  int
	resultSize   int
	errorCount   int
	warningCount int
}

func newHttpRequest(resp http.ResponseWriter, req *http.Request, bp BufferPool) *httpRequest {
	var httpArgs httpRequestArgs

	err := req.ParseForm()

	if req.Method != "GET" && req.Method != "POST" {
		err = fmt.Errorf("Unsupported http method: %s", req.Method)
	}

	if err == nil {
		httpArgs, err = getRequestParams(req)
	}

	var statement string
	if err == nil {
		statement, err = httpArgs.getStatement()
	}

	var prepared *plan.Prepared
	if err == nil {
		prepared, err = getPrepared(httpArgs)
	}

	if err == nil && statement == "" && prepared == nil {
		err = fmt.Errorf("Either statement or prepared must be provided.")
	}

	var namedArgs map[string]value.Value
	if err == nil {
		namedArgs, err = httpArgs.getNamedArgs()
	}

	var positionalArgs value.Values
	if err == nil {
		positionalArgs, err = httpArgs.getPositionalArgs()
	}

	var namespace string
	if err == nil {
		namespace, err = httpArgs.getString(NAMESPACE, "")
	}

	var timeout time.Duration
	if err == nil {
		timeout, err = httpArgs.getDuration(TIMEOUT)
	}

	var readonly value.Tristate
	if err == nil {
		readonly, err = httpArgs.getTristate(READONLY)
	}
	if err == nil && readonly == value.FALSE && req.Method == "GET" {
		err = fmt.Errorf("%s=false cannot be used with HTTP GET method.", READONLY)
	}

	var metrics value.Tristate
	if err == nil {
		metrics, err = httpArgs.getTristate(METRICS)
	}

	var format Format
	if err == nil {
		format, err = getFormat(httpArgs)
	}

	if err == nil && format != JSON {
		err = fmt.Errorf("%s format not yet supported", format)
	}

	var signature value.Tristate
	if err == nil {
		signature, err = httpArgs.getTristate(SIGNATURE)
	}

	var compression Compression
	if err == nil {
		compression, err = getCompression(httpArgs)
	}

	if err == nil && compression != NONE {
		err = fmt.Errorf("%s compression not yet supported", compression)
	}

	var encoding Encoding
	if err == nil {
		encoding, err = getEncoding(httpArgs)
	}

	if err == nil && encoding != UTF8 {
		err = fmt.Errorf("%s encoding not yet supported", encoding)
	}

	var pretty value.Tristate
	if err == nil {
		pretty, err = httpArgs.getTristate(PRETTY)
	}

	if err == nil && pretty == value.FALSE {
		err = fmt.Errorf("false pretty printing not yet supported")
	}

	var consistency *scanConfigImpl

	if err == nil {
		consistency, err = getScanConfiguration(httpArgs)
	}

	var creds datastore.Credentials
	if err == nil {
		creds, err = getCredentials(httpArgs, req.URL.User, req.Header["Authorization"])
	}

	client_id := ""
	if err == nil {
		client_id, err = httpArgs.getString(CLIENT_CONTEXT_ID, "")
	}

	base := server.NewBaseRequest(statement, prepared, namedArgs, positionalArgs,
		namespace, readonly, metrics, signature, consistency, client_id, creds)

	rv := &httpRequest{
		BaseRequest: *base,
		resp:        resp,
		req:         req,
	}

	rv.SetTimeout(rv, timeout)

	rv.writer = NewBufferedWriter(rv, bp)

	// Limit body size in case of denial-of-service attack
	req.Body = http.MaxBytesReader(resp, req.Body, MAX_REQUEST_BYTES)

	// Abort if client closes connection
	closeNotify := resp.(http.CloseNotifier).CloseNotify()
	go func() {
		<-closeNotify
		rv.Stop(server.TIMEOUT)
	}()

	if err != nil {
		rv.Fail(errors.NewError(err, ""))
	}

	return rv
}

const ( // Request argument names
	READONLY          = "readonly"
	METRICS           = "metrics"
	NAMESPACE         = "namespace"
	TIMEOUT           = "timeout"
	ARGS              = "args"
	PREPARED          = "prepared"
	STATEMENT         = "statement"
	FORMAT            = "format"
	ENCODING          = "encoding"
	COMPRESSION       = "compression"
	SIGNATURE         = "signature"
	PRETTY            = "pretty"
	SCAN_CONSISTENCY  = "scan_consistency"
	SCAN_WAIT         = "scan_wait"
	SCAN_VECTOR       = "scan_vector"
	CREDS             = "creds"
	CLIENT_CONTEXT_ID = "client_context_id"
)

func getPrepared(a httpRequestArgs) (*plan.Prepared, error) {
	var prepared *plan.Prepared
	prepared_field, err := a.getValue(PREPARED)
	if err != nil || prepared_field == nil {
		return nil, err
	}

	prepared, err = plan.PreparedCache().GetPrepared(prepared_field)
	if err != nil || prepared != nil {
		return prepared, err
	}

	prepared = &plan.Prepared{}
	json_bytes, err := prepared_field.MarshalJSON()
	if err != nil {
		return nil, err
	}

	err = prepared.UnmarshalJSON(json_bytes)
	if err != nil {
		return nil, err
	}

	err = plan.PreparedCache().AddPrepared(prepared)
	return prepared, err
}

func getCompression(a httpRequestArgs) (Compression, error) {
	var compression Compression

	compression_field, err := a.getString(COMPRESSION, "NONE")
	if err == nil && compression_field != "" {
		compression = newCompression(compression_field)
		if compression == UNDEFINED_COMPRESSION {
			err = fmt.Errorf("Unknown %s value: %s", COMPRESSION, compression)
		}
	}

	return compression, err
}

func getScanConfiguration(a httpRequestArgs) (*scanConfigImpl, error) {
	var sc scanConfigImpl

	scan_consistency_field, err := a.getString(SCAN_CONSISTENCY, "NOT_BOUNDED")
	if err == nil {
		sc.scan_level = newScanConsistency(scan_consistency_field)
		if sc.scan_level == server.UNDEFINED_CONSISTENCY {
			err = fmt.Errorf("Unknown %s value: %s", SCAN_CONSISTENCY, scan_consistency_field)
		}
	}
	if err == nil {
		sc.scan_wait, err = a.getDuration(SCAN_WAIT)
	}
	if err == nil {
		sc.scan_vector, err = a.getScanVector()
	}
	if err == nil && sc.scan_level == server.AT_PLUS && sc.scan_vector == nil {
		err = fmt.Errorf("%s parameter value of AT_PLUS requires %s", SCAN_CONSISTENCY, SCAN_VECTOR)
	}
	return &sc, err
}

func getEncoding(a httpRequestArgs) (Encoding, error) {
	var encoding Encoding

	encoding_field, err := a.getString(ENCODING, "UTF-8")
	if err == nil && encoding_field != "" {
		encoding = newEncoding(encoding_field)
		if encoding == UNDEFINED_ENCODING {
			err = fmt.Errorf("Unknown %s value: %s", ENCODING, encoding)
		}
	}

	return encoding, err
}

func getFormat(a httpRequestArgs) (Format, error) {
	var format Format

	format_field, err := a.getString(FORMAT, "JSON")
	if err == nil && format_field != "" {
		format = newFormat(format_field)
		if format == UNDEFINED_FORMAT {
			err = fmt.Errorf("Unknown %s value: %s", FORMAT, format)
		}
	}

	return format, err
}

func getCredentials(a httpRequestArgs, hdrCreds *url.Userinfo, auths []string) (datastore.Credentials, error) {
	var creds datastore.Credentials

	if hdrCreds != nil {
		// Credentials are in the request URL:
		username := hdrCreds.Username()
		password, _ := hdrCreds.Password()
		creds = make(datastore.Credentials)
		creds[username] = password
		return creds, nil
	}
	if len(auths) > 0 {
		// Credentials are in the request header:
		// TODO: implement non-Basic auth (digest, ntlm)
		auth := auths[0]
		if strings.HasPrefix(auth, "Basic ") {
			encoded_creds := strings.Split(auth, " ")[1]
			decoded_creds, err := base64.StdEncoding.DecodeString(encoded_creds)
			if err != nil {
				return creds, err
			}
			// Authorization header is in format "user:pass"
			// per http://tools.ietf.org/html/rfc1945#section-10.2
			u_details := strings.Split(string(decoded_creds), ":")
			creds = make(datastore.Credentials)
			if len(u_details) == 2 {
				creds[u_details[0]] = u_details[1]
			}
			if len(u_details) == 3 {
				// Support usernames like "local:xxx" or "admin:xxx"
				creds[strings.Join(u_details[:2], ":")] = u_details[2]
			}
		}
		return creds, nil
	}
	// Credentials may be in request arguments:
	cred_data, err := a.getCredentials()
	if err == nil && len(cred_data) > 0 {
		creds = make(datastore.Credentials)
		for _, cred := range cred_data {
			user, user_ok := cred["user"]
			pass, pass_ok := cred["pass"]
			if user_ok && pass_ok {
				creds[user] = pass
			} else {
				err = fmt.Errorf("creds requires both user and pass")
				break
			}
		}
	}
	return creds, err
}

// httpRequestArgs is an interface for getting the arguments in a http request
type httpRequestArgs interface {
	getString(string, string) (string, error)
	getTristate(f string) (value.Tristate, error)
	getValue(field string) (value.Value, error)
	getDuration(string) (time.Duration, error)
	getNamedArgs() (map[string]value.Value, error)
	getPositionalArgs() (value.Values, error)
	getStatement() (string, error)
	getCredentials() ([]map[string]string, error)
	getScanVector() (timestamp.Vector, error)
}

// getRequestParams creates a httpRequestArgs implementation,
// depending on the content type in the request
func getRequestParams(req *http.Request) (httpRequestArgs, error) {

	const (
		URL_CONTENT  = "application/x-www-form-urlencoded"
		JSON_CONTENT = "application/json"
	)
	content_types := req.Header["Content-Type"]
	content_type := URL_CONTENT

	if len(content_types) > 0 {
		content_type = content_types[0]
	}

	if strings.HasPrefix(content_type, URL_CONTENT) {
		return &urlArgs{req: req}, nil
	}

	if strings.HasPrefix(content_type, JSON_CONTENT) {
		return newJsonArgs(req)
	}

	return &urlArgs{req: req}, nil
}

// urlArgs is an implementation of httpRequestArgs that reads
// request arguments from a url-encoded http request
type urlArgs struct {
	req *http.Request
}

func (this *urlArgs) getStatement() (string, error) {
	statement, err := this.formValue(STATEMENT)
	if err != nil {
		return "", err
	}

	if statement == "" && this.req.Method == "POST" {
		bytes, err := ioutil.ReadAll(this.req.Body)
		if err != nil {
			return "", err
		}

		statement = string(bytes)
	}

	return statement, nil
}

// A named argument is an argument of the form: $<identifier>=json_value
func (this *urlArgs) getNamedArgs() (map[string]value.Value, error) {
	var namedArgs map[string]value.Value

	for namedArg, _ := range this.req.Form {
		if !strings.HasPrefix(namedArg, "$") {
			continue
		}
		argString, err := this.formValue(namedArg)
		if err != nil {
			return namedArgs, err
		}
		if len(argString) == 0 {
			//This is an error - there _has_ to be a value for a named argument
			return namedArgs, fmt.Errorf("Named argument %s must have a value", namedArg)
		}
		argValue := value.NewValue([]byte(argString))
		if namedArgs == nil {
			namedArgs = make(map[string]value.Value)
		}
		// NB the '$' is trimmed from the argument name when put in the Value map:
		namedArgs[strings.TrimPrefix(namedArg, "$")] = argValue
	}

	return namedArgs, nil
}

// Positional args are of the form: args=json_list
func (this *urlArgs) getPositionalArgs() (value.Values, error) {
	var positionalArgs value.Values

	args_field, err := this.formValue(ARGS)
	if err != nil || args_field == "" {
		return positionalArgs, err
	}

	var args []interface{}

	decoder := json.NewDecoder(strings.NewReader(args_field))
	err = decoder.Decode(&args)
	if err != nil {
		return positionalArgs, err
	}

	positionalArgs = make([]value.Value, len(args))
	// Put each element of args into positionalArgs
	for i, arg := range args {
		positionalArgs[i] = value.NewValue(arg)
	}

	return positionalArgs, nil
}

func (this *urlArgs) getScanVector() (timestamp.Vector, error) {
	var full_vector_data []*restArg
	var sparse_vector_data map[string]*restArg

	scan_vector_data_field, err := this.formValue(SCAN_VECTOR)

	if err != nil || scan_vector_data_field == "" {
		return nil, err
	}
	decoder := json.NewDecoder(strings.NewReader(scan_vector_data_field))
	err = decoder.Decode(&full_vector_data)
	if err == nil {
		return makeFullVector(full_vector_data)
	}
	decoder = json.NewDecoder(strings.NewReader(scan_vector_data_field))
	err = decoder.Decode(&sparse_vector_data)
	if err != nil {
		return nil, err
	}
	return makeSparseVector(sparse_vector_data)
}

func (this *urlArgs) getDuration(f string) (time.Duration, error) {
	var timeout time.Duration

	timeout_field, err := this.formValue(f)
	if err == nil && timeout_field != "" {
		timeout, err = newDuration(timeout_field)
	}

	return timeout, err
}

func (this *urlArgs) getString(f string, dflt string) (string, error) {
	value := dflt

	value_field, err := this.formValue(f)
	if err == nil && value_field != "" {
		value = value_field
	}

	return value, err
}

func (this *urlArgs) getTristate(f string) (value.Tristate, error) {
	tristate_value := value.NONE
	value_field, err := this.formValue(f)
	if err != nil {
		return tristate_value, err
	}
	if value_field == "" {
		return tristate_value, nil
	}
	bool_value, err := strconv.ParseBool(value_field)
	if err != nil {
		return tristate_value, err
	}
	tristate_value = value.ToTristate(bool_value)
	return tristate_value, err
}

func (this *urlArgs) getCredentials() ([]map[string]string, error) {
	var creds_data []map[string]string

	creds_field, err := this.formValue(CREDS)
	if err == nil && creds_field != "" {
		decoder := json.NewDecoder(strings.NewReader(creds_field))
		err = decoder.Decode(&creds_data)
	}
	return creds_data, err
}

func (this *urlArgs) getValue(field string) (value.Value, error) {
	var val value.Value
	value_field, err := this.getString(field, "")
	if err == nil && value_field != "" {
		val = value.NewValue([]byte(value_field))
	}
	return val, err
}

func (this *urlArgs) formValue(field string) (string, error) {
	values := this.req.Form[field]

	switch len(values) {
	case 0:
		return "", nil
	case 1:
		return values[0], nil
	default:
		return "", fmt.Errorf("Multiple values for field %s.", field)
	}
}

// jsonArgs is an implementation of httpRequestArgs that reads
// request arguments from a json-encoded http request
type jsonArgs struct {
	args map[string]interface{}
	req  *http.Request
}

// create a jsonArgs structure from the given http request.
func newJsonArgs(req *http.Request) (*jsonArgs, error) {
	var p jsonArgs
	decoder := json.NewDecoder(req.Body)
	err := decoder.Decode(&p.args)
	if err != nil {
		return nil, err
	}
	p.req = req
	return &p, nil
}

func (this *jsonArgs) getStatement() (string, error) {
	return this.getString(STATEMENT, "")
}

func (this *jsonArgs) getNamedArgs() (map[string]value.Value, error) {
	var namedArgs map[string]value.Value

	for namedArg, arg := range this.args {
		if strings.HasPrefix(namedArg, "$") {
			// Found a named argument - parse it into a value.Value
			argValue := value.NewValue(arg)
			if namedArgs == nil {
				namedArgs = make(map[string]value.Value)
			}
			namedArgs[namedArg] = argValue
		}
	}

	return namedArgs, nil
}

func (this *jsonArgs) getPositionalArgs() (value.Values, error) {
	var positionalArgs value.Values

	args_field, in_request := this.args[ARGS]

	if !in_request {
		return positionalArgs, nil
	}

	args, type_ok := args_field.([]interface{})

	if !type_ok {
		return positionalArgs, fmt.Errorf("%s parameter has to be an %s", ARGS, "array")
	}

	positionalArgs = make([]value.Value, len(args))
	// Put each element of args into positionalArgs
	for i, arg := range args {
		positionalArgs[i] = value.NewValue(arg)
	}

	return positionalArgs, nil
}

func (this *jsonArgs) getCredentials() ([]map[string]string, error) {
	var creds_data []map[string]string

	creds_field, in_request := this.args[CREDS]

	if !in_request {
		return creds_data, nil
	}

	creds_data, type_ok := creds_field.([]map[string]string)

	if !type_ok {
		return creds_data, fmt.Errorf("%s parameter has to be an %s", CREDS, "array of { user, pass }")
	}

	return creds_data, nil
}

func (this *jsonArgs) getScanVector() (timestamp.Vector, error) {
	var type_ok bool

	scan_vector_data_field, in_request := this.args[SCAN_VECTOR]
	if !in_request {
		return nil, nil
	}
	full_vector_data, type_ok := scan_vector_data_field.([]*restArg)
	if type_ok {
		return makeFullVector(full_vector_data)
	}
	sparse_vector_data, type_ok := scan_vector_data_field.(map[string]*restArg)
	if !type_ok {
		return nil, fmt.Errorf("%s parameter - format not recognised", SCAN_VECTOR)
	}
	return makeSparseVector(sparse_vector_data)
}

func (this *jsonArgs) getDuration(f string) (time.Duration, error) {
	var timeout time.Duration

	t, err := this.getString(f, "0s")

	if err != nil {
		timeout, err = newDuration(t)
	}

	return timeout, err
}

func (this *jsonArgs) getTristate(f string) (value.Tristate, error) {
	value_tristate := value.NONE

	value_field, in_request := this.args[f]

	if !in_request {
		return value_tristate, nil
	}

	b, type_ok := value_field.(bool)

	if !type_ok {
		return value_tristate, fmt.Errorf("%s parameter has to be a %s", f, "boolean")
	}

	value_tristate = value.ToTristate(b)

	return value_tristate, nil
}

// helper function to get a string type argument
func (this *jsonArgs) getString(f string, dflt string) (string, error) {
	value := dflt

	value_field, in_request := this.args[f]

	if !in_request {
		return value, nil
	}

	s, type_ok := value_field.(string)

	if !type_ok {
		return value, fmt.Errorf("%s has to be a %s", f, "string")
	}

	value = s

	return s, nil
}

func (this *jsonArgs) getValue(f string) (value.Value, error) {
	var val value.Value
	value_field, in_request := this.args[f]

	if !in_request {
		return val, nil
	}
	val = value.NewValue(value_field)
	return val, nil
}

type Encoding int

const (
	UTF8 Encoding = iota
	UNDEFINED_ENCODING
)

func newEncoding(s string) Encoding {
	switch strings.ToUpper(s) {
	case "UTF-8":
		return UTF8
	default:
		return UNDEFINED_ENCODING
	}
}

func (e Encoding) String() string {
	var s string
	switch e {
	case UTF8:
		s = "UTF-8"
	default:
		s = "UNDEFINED_ENCODING"
	}
	return s
}

type Format int

const (
	JSON Format = iota
	XML
	CSV
	TSV
	UNDEFINED_FORMAT
)

func newFormat(s string) Format {
	switch strings.ToUpper(s) {
	case "JSON":
		return JSON
	case "XML":
		return XML
	case "CSV":
		return CSV
	case "TSV":
		return TSV
	default:
		return UNDEFINED_FORMAT
	}
}

func (f Format) String() string {
	var s string
	switch f {
	case JSON:
		s = "JSON"
	case XML:
		s = "XML"
	case CSV:
		s = "CSV"
	case TSV:
		s = "TSV"
	default:
		s = "UNDEFINED_FORMAT"
	}
	return s
}

type Compression int

const (
	NONE Compression = iota
	ZIP
	RLE
	LZMA
	LZO
	UNDEFINED_COMPRESSION
)

func newCompression(s string) Compression {
	switch strings.ToUpper(s) {
	case "NONE":
		return NONE
	case "ZIP":
		return ZIP
	case "RLE":
		return RLE
	case "LZMA":
		return LZMA
	case "LZO":
		return LZO
	default:
		return UNDEFINED_COMPRESSION
	}
}

func (c Compression) String() string {
	var s string
	switch c {
	case NONE:
		s = "NONE"
	case ZIP:
		s = "ZIP"
	case RLE:
		s = "RLE"
	case LZMA:
		s = "LZMA"
	case LZO:
		s = "LZO"
	default:
		s = "UNDEFINED_COMPRESSION"
	}
	return s
}

// scanVectorEntry implements timestamp.Entry
type scanVectorEntry struct {
	pos  uint32
	val  uint64
	uuid string
}

func (this *scanVectorEntry) Position() uint32 {
	return this.pos
}

func (this *scanVectorEntry) Value() uint64 {
	return this.val
}

func (this *scanVectorEntry) Validation() string {
	return this.uuid
}

// scanVectorEntries implements timestamp.Vector
type scanVectorEntries struct {
	entries []timestamp.Entry
}

func (this *scanVectorEntries) Entries() []timestamp.Entry {
	return this.entries
}

// restArg captures how vector data is passed via REST
type restArg struct {
	Seqno uint64 `json:"seqno"`
	Uuid  string `json:"uuid"`
}

// makeFullVector is used when the request includes all entries
func makeFullVector(args []*restArg) (*scanVectorEntries, error) {
	if len(args) != SCAN_VECTOR_SIZE {
		return nil,
			fmt.Errorf("%s parameter has to contain %d sequence numbers",
				SCAN_VECTOR, SCAN_VECTOR_SIZE)
	}
	entries := make([]timestamp.Entry, len(args))
	for i, arg := range args {
		entries[i] = &scanVectorEntry{
			pos:  uint32(i),
			val:  arg.Seqno,
			uuid: arg.Uuid,
		}
	}
	return &scanVectorEntries{
		entries: entries,
	}, nil
}

// makeSparseVector is used when the request contains a sparse entry arg
func makeSparseVector(args map[string]*restArg) (*scanVectorEntries, error) {
	entries := make([]timestamp.Entry, len(args))
	i := 0
	for key, arg := range args {
		index, err := strconv.Atoi(key)
		if err != nil {
			return nil, err
		}
		entries[i] = &scanVectorEntry{
			pos:  uint32(index),
			val:  arg.Seqno,
			uuid: arg.Uuid,
		}
		i = i + 1
	}
	return &scanVectorEntries{
		entries: entries,
	}, nil
}

const SCAN_VECTOR_SIZE = 1024

type scanConfigImpl struct {
	scan_level  server.ScanConsistency
	scan_wait   time.Duration
	scan_vector timestamp.Vector
}

func (this *scanConfigImpl) ScanConsistency() datastore.ScanConsistency {
	switch this.scan_level {
	case server.NOT_BOUNDED:
		return datastore.UNBOUNDED
	case server.REQUEST_PLUS, server.STATEMENT_PLUS:
		return datastore.SCAN_PLUS
	case server.AT_PLUS:
		return datastore.AT_PLUS
	default:
		return datastore.UNBOUNDED
	}
}

func (this *scanConfigImpl) ScanWait() time.Duration {
	return this.scan_wait
}

func (this *scanConfigImpl) ScanVector() timestamp.Vector {
	return this.scan_vector
}

func newScanConsistency(s string) server.ScanConsistency {
	switch strings.ToUpper(s) {
	case "NOT_BOUNDED":
		return server.NOT_BOUNDED
	case "REQUEST_PLUS":
		return server.REQUEST_PLUS
	case "STATEMENT_PLUS":
		return server.STATEMENT_PLUS
	case "AT_PLUS":
		return server.AT_PLUS
	default:
		return server.UNDEFINED_CONSISTENCY
	}
}

// helper function to create a time.Duration instance from a given string.
// There must be a unit - valid units are "ns", "us", "ms", "s", "m", "h"
func newDuration(s string) (time.Duration, error) {
	var duration time.Duration
	var err error
	// Error if given string has no unit
	last_char := s[len(s)-1]
	if last_char != 's' && last_char != 'm' && last_char != 'h' {
		err = errors.NewError(nil,
			fmt.Sprintf("Missing or incorrect unit for duration: "+
				"%s (valid units: ns, us, ms, s, m, h)", s))
	}
	if err == nil {
		duration, err = time.ParseDuration(s)
	}
	return duration, err
}
