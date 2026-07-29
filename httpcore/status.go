package httpcore

// Status codes PulseHTTP knows how to emit. The server is deliberate about the
// distinctions the RFC cares about: 401 (who are you?) vs 403 (I know who you
// are, and no), 404 (no such resource) vs 405 (resource exists, wrong verb),
// 408 (you were too slow) vs 429 (you were too fast).
const (
	StatusOK                  = 200
	StatusCreated             = 201
	StatusNoContent           = 204
	StatusMovedPermanently    = 301
	StatusNotModified         = 304
	StatusBadRequest          = 400
	StatusUnauthorized        = 401
	StatusForbidden           = 403
	StatusNotFound            = 404
	StatusMethodNotAllowed    = 405
	StatusRequestTimeout      = 408
	StatusLengthRequired      = 411
	StatusPayloadTooLarge     = 413
	StatusURITooLong          = 414
	StatusTooManyRequests     = 429
	StatusHeaderFieldsTooLarge = 431
	StatusInternalServerError = 500
	StatusNotImplemented      = 501
	StatusBadGateway          = 502
	StatusServiceUnavailable  = 503
	StatusGatewayTimeout      = 504
	StatusHTTPVersionNotSupported = 505
)

var reasonPhrases = map[int]string{
	100: "Continue",
	200: "OK",
	201: "Created",
	202: "Accepted",
	204: "No Content",
	301: "Moved Permanently",
	302: "Found",
	304: "Not Modified",
	400: "Bad Request",
	401: "Unauthorized",
	403: "Forbidden",
	404: "Not Found",
	405: "Method Not Allowed",
	408: "Request Timeout",
	409: "Conflict",
	411: "Length Required",
	413: "Payload Too Large",
	414: "URI Too Long",
	415: "Unsupported Media Type",
	429: "Too Many Requests",
	431: "Request Header Fields Too Large",
	500: "Internal Server Error",
	501: "Not Implemented",
	502: "Bad Gateway",
	503: "Service Unavailable",
	504: "Gateway Timeout",
	505: "HTTP Version Not Supported",
}

// ReasonPhrase returns the canonical reason phrase for a status code.
func ReasonPhrase(code int) string {
	if p, ok := reasonPhrases[code]; ok {
		return p
	}
	return "Status"
}
