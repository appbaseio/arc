package logs

import (
	"encoding/json"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/appbaseio/reactivesearch-api/middleware"
	"github.com/appbaseio/reactivesearch-api/middleware/classify"
	"github.com/appbaseio/reactivesearch-api/middleware/validate"
	"github.com/appbaseio/reactivesearch-api/model/category"
	"github.com/appbaseio/reactivesearch-api/model/index"
	"github.com/appbaseio/reactivesearch-api/model/request"
	"github.com/appbaseio/reactivesearch-api/plugins/auth"
	"github.com/appbaseio/reactivesearch-api/util"
	"github.com/buger/jsonparser"
	log "github.com/sirupsen/logrus"
)

type chain struct {
	middleware.Fifo
}

// SearchResponseBody represents the response body returned by search
type SearchResponseBody struct {
	Took float64 `json:"took"`
}

// RSSettings represents the settings object in RS API response
type RSSettings struct {
	Took float64 `json:"took"`
}

// ResponseBodyRS represents the response body returned by reactivesearch route
type ResponseBodyRS struct {
	Settings RSSettings `json:"settings"`
}

func (c *chain) Wrap(h http.HandlerFunc) http.HandlerFunc {
	return c.Adapt(h, list()...)
}

func list() []middleware.Middleware {
	return []middleware.Middleware{
		classifyCategory,
		classify.Op(),
		classify.Indices(),
		auth.BasicAuth(),
		validate.Indices(),
		validate.Operation(),
		validate.Category(),
	}
}

func classifyCategory(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		requestCategory := category.Logs

		ctx := category.NewContext(req.Context(), &requestCategory)
		req = req.WithContext(ctx)

		h(w, req)
	}
}

type Request struct {
	URI     string              `json:"uri"`
	Method  string              `json:"method"`
	Headers map[string][]string `json:"header"`
	Body    string              `json:"body"`
}

type Response struct {
	Code    int    `json:"code"`
	Status  string `json:"status"`
	Headers map[string][]string
	Took    *float64 `json:"took,omitempty"`
	Body    string   `json:"body"`
}

type record struct {
	Indices   []string          `json:"indices"`
	Category  category.Category `json:"category"`
	Request   Request           `json:"request"`
	Response  Response          `json:"response"`
	Timestamp time.Time         `json:"timestamp"`
}

// Recorder records a log "record" for every request.
func Recorder() middleware.Middleware {
	return Instance().recorder
}

func (l *Logs) recorder(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// skip logs from streams
		if r.Header.Get("X-Request-Category") == "streams" {
			h(w, r)
			return
		}
		ctx := r.Context()

		reqCategory, err := category.FromContext(ctx)
		if err != nil {
			log.Errorln(logTag, ":", err)
			return
		}

		var dumpRequest []byte
		if *reqCategory != category.ReactiveSearch {
			dumpRequest, err = httputil.DumpRequest(r, true)
			if err != nil {
				log.Errorln(logTag, ":", err.Error())
				return
			}
		}
		// Serve using response recorder
		respRecorder := httptest.NewRecorder()
		h(respRecorder, r)
		// Copy the response to writer
		for k, v := range respRecorder.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(respRecorder.Code)
		w.Write(respRecorder.Body.Bytes())
		// Record the document

		go l.recordResponse(respRecorder, r, dumpRequest)
	}
}

func (l *Logs) recordResponse(w *httptest.ResponseRecorder, r *http.Request, reqBody []byte) {
	var headers = make(map[string][]string)

	for key, values := range r.Header {
		headers[key] = values
	}

	ctx := r.Context()

	reqCategory, err := category.FromContext(ctx)
	if err != nil {
		log.Errorln(logTag, ":", err)
		return
	}

	reqIndices, err := index.FromContext(ctx)
	if err != nil {
		log.Errorln(logTag, ":", err)
		return
	}

	var rec record
	rec.Indices = reqIndices
	rec.Category = *reqCategory
	rec.Timestamp = time.Now()

	// record response
	response := w.Result()
	rec.Response.Code = response.StatusCode
	rec.Response.Status = http.StatusText(response.StatusCode)
	rec.Response.Headers = response.Header

	responseBody, err := ioutil.ReadAll(response.Body)
	if err != nil {
		log.Errorln(logTag, "can't read response body: ", err)
		return
	}
	if *reqCategory == category.Search {
		var resBody SearchResponseBody
		err := json.Unmarshal(responseBody, &resBody)
		if err != nil {
			log.Errorln(logTag, "error encountered while parsing the response: ", err)
		}
		// ignore error to record error logs
		if err == nil {
			rec.Response.Took = &resBody.Took
		}
	}
	if *reqCategory == category.ReactiveSearch {
		// Read request body from context
		rsRequestBody, err := request.FromContext(ctx)
		// ignore error to record > 500 status code logs
		if err != nil {
			log.Errorln(logTag, "error encountered while reading request body:", err)
		}
		marshalled, err := json.Marshal(rsRequestBody)
		if err != nil {
			log.Errorln(logTag, "error encountered while marshalling request body:", err)
			return
		}
		rec.Request = Request{
			URI:     r.URL.Path,
			Headers: headers,
			Body:    string(marshalled[:util.Min(len(marshalled), 1000000)]),
			Method:  r.Method,
		}
		// read success response from context
		tookValue, err := jsonparser.GetFloat(w.Body.Bytes(), "settings", "took")
		if err != nil {
			log.Errorln(logTag, "error encountered while reading took key from response body:", err)
		} else {
			// Set took value
			rec.Response.Took = &tookValue
		}
		// read error response from response recorder body
		rec.Response.Body = string(responseBody[:util.Min(len(responseBody), 1000000)])
	} else {
		requestBody := strings.Split(string(reqBody), "\r\n\r\n")
		var parsedBody []byte
		if len(requestBody) > 1 {
			parsedBody = []byte(requestBody[1])
		}
		// record request
		rec.Request = Request{
			URI:     r.URL.Path,
			Headers: headers,
			Body:    string(parsedBody[:util.Min(len(parsedBody), 1000000)]),
			Method:  r.Method,
		}
		rec.Response.Body = string(responseBody[:util.Min(len(responseBody), 1000000)])
	}
	marshalledLog, err := json.Marshal(rec)
	if err != nil {
		log.Errorln(logTag, "error encountered while marshalling record :", err)
		return
	}
	n, err := l.lumberjack.Write(marshalledLog)
	if err != nil {
		log.Errorln(logTag, "error encountered while writing logs :", err)
		return
	}
	// Add new line character so filebeat can sync it with ES
	l.lumberjack.Write([]byte("\n"))
	log.Println(logTag, "logged request successfully", n)
}
