package gatewayhttp

import "net/url"

// pathUnescape decodes one path segment of a model id (an inference profile ARN arrives with %3A and
// %2F).
func pathUnescape(s string) (string, error) { return url.PathUnescape(s) }
