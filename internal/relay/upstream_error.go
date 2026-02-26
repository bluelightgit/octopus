package relay

// upstreamHTTPError carries an upstream HTTP response payload that should be
// returned to the client when no other channels succeed.
//
// It is intentionally protocol-agnostic: the body is already in the upstream
// protocol format (OpenAI/Anthropic/etc).
type upstreamHTTPError struct {
	StatusCode  int
	ContentType string
	Body        []byte
}

func (e upstreamHTTPError) Error() string {
	return "upstream HTTP error"
}
