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
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/couchbase/query/errors"
	"github.com/couchbase/query/execution"
	"github.com/couchbase/query/logging"
	"github.com/couchbase/query/server"
	"github.com/couchbase/query/value"
)

const (
	PRETTY_RESULT_PREFIX string = "        "
	PRETTY_RESULT_INDENT string = "    "
	PRETTY_PREFIX        string = "    "
	PRETTY_INDENT        string = "    "
	NO_PRETTY_PREFIX     string = ""
	NO_PRETTY_INDENT     string = ""
)

func (this *httpRequest) Output() execution.Output {
	return this
}

func (this *httpRequest) Fail(err errors.Error) {
	this.SetState(server.FATAL)
	// Determine the appropriate http response code based on the error
	httpRespCode := mapErrorToHttpResponse(err, http.StatusInternalServerError)
	this.setHttpCode(httpRespCode)
	// Add error to the request
	this.Error(err)
}

func mapErrorToHttpResponse(err errors.Error, def int) int {

	// MB-19307: please note that setting the http status
	// only works if the http header has not been sent.
	// This is the case if the whole output document is
	// smaller than the threshold beyond which the http
	// server starts sending the output with a chunked
	// transfer encoding, or the first chunk has not been
	// put together yet.
	// For this reason, be mindful that error codes mapped
	// here should only be generated at a point in which
	// the request has not produced any results (ie failed
	// in some sort of non starter way)
	switch err.Code() {
	case 1000: // readonly violation
		return http.StatusForbidden
	case 1010: // unsupported http method
		return http.StatusMethodNotAllowed
	case 1020, 1030, 1040, 1050, 1060, 1065, 1070:
		return http.StatusBadRequest
	case 1120:
		return http.StatusNotAcceptable
	case 3000: // parse error range
		return http.StatusBadRequest
	case 4000, errors.NO_SUCH_PREPARED: // plan error range
		return http.StatusNotFound
	case 4300:
		return http.StatusConflict
	case 5000:
		return http.StatusInternalServerError
	case 10000:
		return http.StatusUnauthorized
	default:
		return def
	}
}

func (this *httpRequest) httpCode() int {
	this.RLock()
	defer this.RUnlock()
	return this.httpRespCode
}

func (this *httpRequest) setHttpCode(httpRespCode int) {
	this.Lock()
	defer this.Unlock()
	this.httpRespCode = httpRespCode
}

func (this *httpRequest) Failed(srvr *server.Server) {
	defer this.stopAndAlert(server.FATAL)

	prefix, indent := this.prettyStrings(srvr.Pretty(), false)
	this.writeString("{\n")
	this.writeRequestID(prefix)
	this.writeClientContextID(prefix)
	this.writeErrors(prefix, indent)
	this.writeWarnings(prefix, indent)
	this.writeState("", prefix)

	this.markTimeOfCompletion(time.Now())

	this.writeMetrics(srvr.Metrics(), prefix, indent)
	this.writeProfile(srvr.Profile(), prefix, indent)
	this.writeControls(srvr.Controls(), prefix, indent)
	this.writeString("\n}\n")
	this.writer.noMoreData()
}

func (this *httpRequest) markTimeOfCompletion(now time.Time) {
	this.executionTime = now.Sub(this.ServiceTime())
	this.elapsedTime = now.Sub(this.RequestTime())
}

func (this *httpRequest) Execute(srvr *server.Server, signature value.Value) {
	var stopped bool
	this.prefix, this.indent = this.prettyStrings(srvr.Pretty(), false)

	this.setHttpCode(http.StatusOK)
	this.writePrefix(srvr, signature, this.prefix, this.indent)

	// release writer
	this.Done()

	// wait for somebody to tell us we're done, or toast
	select {
	case <-this.Results():
		this.SetState(server.COMPLETED)
		stopped = false
	case <-this.StopExecute():
		this.SetState(server.STOPPED)

		// wait for operator before continuing
		<-this.Results()
		stopped = true
	case <-this.httpCloseNotify:
		this.SetState(server.CLOSED)

		// wait for operator before continuing
		<-this.Results()
		stopped = false
	}

	now := time.Now()
	this.Output().AddPhaseTime(execution.RUN, now.Sub(this.ExecTime()))
	this.markTimeOfCompletion(now)

	state := this.State()
	this.writeSuffix(srvr, state, this.prefix, this.indent)
	this.writer.noMoreData()
	if !stopped {
		this.Stop(server.COMPLETED)
	}
	this.Alert()
}

func (this *httpRequest) Expire(state server.State, timeout time.Duration) {
	this.Error(errors.NewTimeoutError(timeout))
	this.Stop(state)
}

func (this *httpRequest) stopAndAlert(state server.State) {
	this.Stop(state)
	this.Alert()
}

func (this *httpRequest) writePrefix(srvr *server.Server, signature value.Value, prefix, indent string) bool {
	return this.writeString("{\n") &&
		this.writeRequestID(prefix) &&
		this.writeClientContextID(prefix) &&
		this.writeSignature(srvr.Signature(), signature, prefix, indent) &&
		this.writeString(",\n") &&
		this.writeString(prefix) &&
		this.writeString("\"results\": [")
}

func (this *httpRequest) writeRequestID(prefix string) bool {
	return this.writeString(prefix) && this.writeString("\"requestID\": \"") && this.writeString(this.Id().String()) && this.writeString("\"")
}

func (this *httpRequest) writeClientContextID(prefix string) bool {
	if !this.ClientID().IsValid() {
		return true
	}
	return this.writeString(",\n") && this.writeString(prefix) &&
		this.writeString("\"clientContextID\": \"") && this.writeString(this.ClientID().String()) && this.writeString("\"")
}

func (this *httpRequest) writeSignature(server_flag bool, signature value.Value, prefix, indent string) bool {
	s := this.Signature()
	if s == value.FALSE || (s == value.NONE && !server_flag) {
		return true
	}
	return this.writeString(",\n") && this.writeString(prefix) && this.writeString("\"signature\": ") && this.writeValue(signature, prefix, indent)
}

func (this *httpRequest) prettyStrings(serverPretty, result bool) (string, string) {
	p := this.Pretty()
	if p == value.FALSE || (p == value.NONE && !serverPretty) {
		return NO_PRETTY_PREFIX, NO_PRETTY_INDENT
	} else if result {
		return PRETTY_RESULT_PREFIX, PRETTY_RESULT_INDENT
	} else {
		return PRETTY_PREFIX, PRETTY_INDENT
	}
}

func (this *httpRequest) SetUp() {

	// wait for prefix
	this.Wait()
}

func (this *httpRequest) Result(item value.AnnotatedValue) bool {
	var success bool

	if this.Halted() {
		return false
	}

	this.writer.timeFlush()
	beforeWrites := this.writer.mark()

	if this.resultCount == 0 {
		success = this.writer.write("\n")
	} else {
		success = this.writer.write(",\n")
	}
	if success {
		success = this.writer.write(this.prefix)
	}
	beforeResult := this.writer.mark()

	if success {
		err := item.WriteJSON(this.writer.buf(), this.prefix, this.indent, item.Self())
		if err != nil {
			this.Error(errors.NewServiceErrorInvalidJSON(err))
			this.SetState(server.FATAL)
			success = false
		} else {
			this.resultSize += (this.writer.mark() - beforeResult)
			this.resultCount++
			this.writer.sizeFlush()
		}
	} else {
		this.SetState(server.CLOSED)
	}

	// did not work out: remove last writes so that we have a well formed document
	if !success {
		this.writer.truncate(beforeWrites)
	}
	return success
}

func (this *httpRequest) writeValue(item value.Value, prefix, indent string) bool {
	if item == nil {
		return this.writeString("null")
	}
	beforeWriteJSON := this.writer.mark()
	err := item.WriteJSON(this.writer.buf(), prefix, indent, false)
	if err != nil {
		this.writer.truncate(beforeWriteJSON)
		return this.writeString(fmt.Sprintf("\"ERROR: %v\"", err))
	}
	return true
}

func (this *httpRequest) writeSuffix(srvr *server.Server, state server.State, prefix, indent string) bool {
	return this.writeString("\n") && this.writeString(prefix) && this.writeString("]") &&
		this.writeErrors(prefix, indent) &&
		this.writeWarnings(prefix, indent) &&
		this.writeState(state, prefix) &&
		this.writeMetrics(srvr.Metrics(), prefix, indent) &&
		this.writeProfile(srvr.Profile(), prefix, indent) &&
		this.writeControls(srvr.Controls(), prefix, indent) &&
		this.writeString("\n}\n")
}

func (this *httpRequest) writeString(s string) bool {
	return this.writer.writeString(s)
}

func (this *httpRequest) writeState(state server.State, prefix string) bool {
	if state == "" {
		state = this.State()
	}

	if state == server.COMPLETED {
		if this.errorCount == 0 {
			state = server.SUCCESS
		} else {
			state = server.ERRORS
		}
	}

	return this.writeString(fmt.Sprintf(",\n%s\"status\": \"%s\"", prefix, state))
}

func (this *httpRequest) writeErrors(prefix string, indent string) bool {
	var err errors.Error
	for _, err = range this.Errors() {
		if this.errorCount == 0 {
			this.writeString(",\n")
			this.writeString(prefix)
			this.writeString("\"errors\": [")

			// MB-19307: please check the comments
			// in mapErrortoHttpResponse().
			// Ideally we should set the status code
			// only before calling writePrefix()
			// but this is too cumbersome, having
			// to check Execution errors as well.
			if this.State() != server.FATAL {
				this.setHttpCode(mapErrorToHttpResponse(err, http.StatusOK))
			}
		}
		if !this.writeError(err, this.errorCount, prefix, indent) {
			break
		}
		this.errorCount++
	}

	if this.errorCount == 0 {
		return true
	}

	if prefix != "" && !(this.writeString("\n") && this.writeString(prefix)) {
		return false
	}
	return this.writeString("]")
}

func (this *httpRequest) writeWarnings(prefix, indent string) bool {
	var err errors.Error
	alreadySeen := make(map[string]bool)

loop:
	for _, err = range this.Warnings() {
		if err.OnceOnly() && alreadySeen[err.Error()] {
			// do nothing for this warning
			continue loop
		}
		if this.warningCount == 0 {
			this.writeString(",\n")
			this.writeString(prefix)
			this.writeString("\"warnings\": [")
		}
		if !this.writeError(err, this.warningCount, prefix, indent) {
			break
		}
		this.warningCount++
		alreadySeen[err.Error()] = true
	}

	if this.warningCount == 0 {
		return true
	}

	if prefix != "" && !(this.writeString("\n") && this.writeString(prefix)) {
		return false
	}
	return this.writeString("]")
}

func (this *httpRequest) writeError(err errors.Error, count int, prefix, indent string) bool {

	newPrefix := prefix + indent

	if count != 0 && !this.writeString(",") {
		return false
	}
	if prefix != "" && !this.writeString("\n") {
		return false
	}

	m := map[string]interface{}{
		"code": err.Code(),
		"msg":  err.Error(),
	}

	var er error
	var bytes []byte

	if newPrefix == "" && indent == "" {
		bytes, er = json.Marshal(m)
	} else {
		bytes, er = json.MarshalIndent(m, newPrefix, indent)
	}
	if er != nil {
		return false
	}

	return this.writeString(newPrefix) && this.writeString(string(bytes))
}

func (this *httpRequest) writeMetrics(metrics bool, prefix, indent string) bool {
	m := this.Metrics()
	if m == value.FALSE || (m == value.NONE && !metrics) {
		return true
	}

	var newPrefix string
	if prefix != "" {
		newPrefix = "\n" + prefix + indent
	}

	beforeMetrics := this.writer.mark()
	if !(this.writeString(",\n") && this.writeString(prefix) && this.writeString("\"metrics\": {")) {
		this.writer.truncate(beforeMetrics)
		return false
	}
	buf := this.writer.buf()
	fmt.Fprintf(buf, "%s\"elapsedTime\": \"%v\"", newPrefix, this.elapsedTime)
	fmt.Fprintf(buf, ",%s\"executionTime\": \"%v\"", newPrefix, this.executionTime)
	fmt.Fprintf(buf, ",%s\"resultCount\": %d", newPrefix, this.resultCount)
	fmt.Fprintf(buf, ",%s\"resultSize\": %d", newPrefix, this.resultSize)

	if this.MutationCount() > 0 {
		fmt.Fprintf(buf, ",%s\"mutationCount\": %d", newPrefix, this.MutationCount())
	}

	if this.SortCount() > 0 {
		fmt.Fprintf(buf, ",%s\"sortCount\": %d", newPrefix, this.SortCount())
	}

	if this.errorCount > 0 {
		fmt.Fprintf(buf, ",%s\"errorCount\": %d", newPrefix, this.errorCount)
	}

	if this.warningCount > 0 {
		fmt.Fprintf(buf, ",%s\"warningCount\": %d", newPrefix, this.warningCount)
	}

	if prefix != "" && !(this.writeString("\n") && this.writeString(prefix)) {
		this.writer.truncate(beforeMetrics)
		return false
	}
	return this.writeString("}")
}

func (this *httpRequest) writeControls(controls bool, prefix, indent string) bool {
	var newPrefix string
	var e []byte
	var err error

	needComma := false
	c := this.Controls()
	if c == value.FALSE || (c == value.NONE && !controls) {
		return true
	}

	namedArgs := this.NamedArgs()
	positionalArgs := this.PositionalArgs()
	if namedArgs == nil && positionalArgs == nil {
		return true
	}

	if prefix != "" {
		newPrefix = "\n" + prefix + indent
	}
	rv := this.writeString(",\n") && this.writeString(prefix) && this.writeString("\"controls\": {")
	if !rv {
		return false
	}
	if namedArgs != nil {
		if indent != "" {
			e, err = json.MarshalIndent(namedArgs, "\t", indent)
		} else {
			e, err = json.Marshal(namedArgs)
		}
		if err != nil || !this.writeString(fmt.Sprintf("%s\"namedArgs\": %s", newPrefix, e)) {
			logging.Infop("Error writing namedArgs", logging.Pair{"error", err})
		}
		needComma = true
	}
	if positionalArgs != nil {
		if needComma && !this.writeString(",") {
			return false
		}
		if indent != "" {
			e, err = json.MarshalIndent(positionalArgs, "\t", indent)
		} else {
			e, err = json.Marshal(positionalArgs)
		}
		if err != nil || !this.writeString(fmt.Sprintf("%s\"positionalArgs\": %s", newPrefix, e)) {
			logging.Infop("Error writing positional args", logging.Pair{"error", err})
		}
	}
	if prefix != "" && !(this.writeString("\n") && this.writeString(prefix)) {
		return false
	}
	return this.writeString("}")
}

func (this *httpRequest) writeProfile(profile server.Profile, prefix, indent string) bool {
	var newPrefix string
	var e []byte
	var err error

	needComma := false
	p := this.Profile()
	if p == server.ProfUnset {
		p = profile
	}
	if p == server.ProfOff {
		return true
	}

	if prefix != "" {
		newPrefix = "\n" + prefix + indent
	}
	rv := this.writeString(",\n") && this.writeString(prefix) && this.writeString("\"profile\": {")
	if !rv {
		return false
	}
	if p != server.ProfOff {
		phaseTimes := this.FmtPhaseTimes()
		if phaseTimes != nil {
			if indent != "" {
				e, err = json.MarshalIndent(phaseTimes, "\t", indent)
			} else {
				e, err = json.Marshal(phaseTimes)
			}
			if err != nil || !this.writeString(fmt.Sprintf("%s\"phaseTimes\": %s", newPrefix, e)) {
				logging.Infop("Error writing phase times", logging.Pair{"error", err})
			}
			needComma = true
		}
		phaseCounts := this.FmtPhaseCounts()
		if phaseCounts != nil {
			if needComma && !this.writeString(",") {
				return false
			}
			if indent != "" {
				e, err = json.MarshalIndent(phaseCounts, "\t", indent)
			} else {
				e, err = json.Marshal(phaseCounts)
			}
			if err != nil || !this.writeString(fmt.Sprintf("%s\"phaseCounts\": %s", newPrefix, e)) {
				logging.Infop("Error writing phase counts", logging.Pair{"error", err})
			}
			needComma = true
		}
		phaseOperators := this.FmtPhaseOperators()
		if phaseOperators != nil {
			if needComma && !this.writeString(",") {
				return false
			}
			if indent != "" {
				e, err = json.MarshalIndent(phaseOperators, "\t", indent)
			} else {
				e, err = json.Marshal(phaseOperators)
			}
			if err != nil || !this.writeString(fmt.Sprintf("%s\"phaseOperators\": %s", newPrefix, e)) {
				logging.Infop("Error writing phase operators", logging.Pair{"error", err})
			}
		}
	}
	if p == server.ProfOn {
		timings := this.GetTimings()
		if timings != nil {
			if indent != "" {
				e, err = json.MarshalIndent(timings, "\t", indent)
			} else {
				e, err = json.Marshal(timings)
			}
			if err != nil || !this.writeString(fmt.Sprintf(",%s\"executionTimings\": %s", newPrefix, e)) {
				logging.Infop("Error writing timings", logging.Pair{"error", err})
			}
		}
	}
	if prefix != "" && !(this.writeString("\n") && this.writeString(prefix)) {
		return false
	}
	return this.writeString("}")
}

// bufferedWriter is an implementation of responseDataManager that writes response data to a buffer,
// up to a threshold:
type bufferedWriter struct {
	sync.Mutex
	req         *httpRequest  // the request for the response we are writing
	buffer      *bytes.Buffer // buffer for writing response data to
	buffer_pool BufferPool    // buffer manager for our buffers
	closed      bool
	header      bool // headers required
	lastFlush   time.Time
}

func NewBufferedWriter(w *bufferedWriter, r *httpRequest, bp BufferPool) {
	w.req = r
	w.buffer = bp.GetBuffer()
	w.buffer_pool = bp
	w.closed = false
	w.header = true
	w.lastFlush = time.Now()
}

func (this *bufferedWriter) writeString(s string) bool {
	if this.closed {
		return false
	}

	this.Lock()
	defer this.Unlock()

	// threshold exceeded
	if len(s)+this.buffer.Len() > this.buffer_pool.BufferCapacity() {
		w := this.req.resp // our request's response writer

		// write response header and data buffered so far using request's response writer:
		if this.header {
			w.WriteHeader(this.req.httpCode())
			this.header = false
		}

		// write out and empty the buffer
		io.Copy(w, this.buffer)
		this.buffer.Reset()

		// do the flushing
		this.lastFlush = time.Now()
		w.(http.Flusher).Flush()
	}

	// under threshold - write the string to our buffer
	_, err := this.buffer.Write([]byte(s))
	return err == nil
}

// these are only used by Result() handling
// fast write
func (this *bufferedWriter) write(s string) bool {
	_, err := this.buffer.Write([]byte(s))
	return err == nil
}

// flush in a timely manner
func (this *bufferedWriter) timeFlush() {

	// time flushing only happens after we have sent the first buffer
	if this.closed || this.header {
		return
	}

	this.Lock()
	defer this.Unlock()

	// flush only if time has exceeded
	if time.Since(this.lastFlush) > 100*time.Millisecond {
		w := this.req.resp // our request's response writer

		// write response header and data buffered so far using request's response writer:
		if this.header {
			w.WriteHeader(this.req.httpCode())
			this.header = false
		}

		// write out and empty the buffer
		io.Copy(w, this.buffer)
		this.buffer.Reset()

		// do the flushing
		this.lastFlush = time.Now()
		w.(http.Flusher).Flush()
	}
}

// flush on a full buffer
func (this *bufferedWriter) sizeFlush() {
	if this.closed {
		return
	}

	this.Lock()
	defer this.Unlock()

	// beyond capacity
	if this.buffer.Len() > this.buffer_pool.BufferCapacity() {
		w := this.req.resp // our request's response writer

		// write response header and data buffered so far using request's response writer:
		if this.header {
			w.WriteHeader(this.req.httpCode())
			this.header = false
		}

		// write out and empty the buffer
		io.Copy(w, this.buffer)
		this.buffer.Reset()

		// do the flushing
		this.lastFlush = time.Now()
		w.(http.Flusher).Flush()
	}
}

// mark the current write position
func (this *bufferedWriter) mark() int {
	return this.buffer.Len()
}

func (this *bufferedWriter) truncate(mark int) {
	this.buffer.Truncate(mark)
}

func (this bufferedWriter) buf() io.Writer {
	return this.buffer
}

// empty and dispose of writer
func (this *bufferedWriter) noMoreData() {
	this.Lock()
	defer this.Unlock()

	if this.closed {
		return
	}

	w := this.req.resp // our request's response writer
	r := this.req.req  // our request's http request

	if this.header {
		// calculate and set the Content-Length header:
		content_len := strconv.Itoa(len(this.buffer.Bytes()))
		w.Header().Set("Content-Length", content_len)
		// write response header and data buffered so far:
		w.WriteHeader(this.req.httpCode())
		this.header = false
	}

	io.Copy(w, this.buffer)
	// no more data in the response => return buffer to pool:
	this.buffer_pool.PutBuffer(this.buffer)
	r.Body.Close()
	this.closed = true
}
