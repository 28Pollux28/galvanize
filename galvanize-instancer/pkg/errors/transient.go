package errors

import "strings"

// TransientErrorPatterns contains patterns that indicate transient errors worth retrying.
// These include SSH connection issues, network timeouts, and Docker registry errors.
var TransientErrorPatterns = []string{
	// SSH errors
	"Session open refused by peer",
	"no more sessions",
	"Connection closed by",
	"mux_client_request_session",
	"connection refused",
	"Connection reset by peer",
	"Data could not be sent to remote host",
	"Unreachable: 1",
	// Docker/network errors
	"context deadline exceeded",
	"connection timed out",
	"i/o timeout",
	"TLS handshake timeout",
	"no such host",
	"network is unreachable",
}

// IsTransientError checks if the error message or output contains a transient error pattern.
func IsTransientError(err error, output string) (bool, string) {
	// Check error message
	if err != nil {
		msg := err.Error()
		for _, pattern := range TransientErrorPatterns {
			if strings.Contains(msg, pattern) {
				return true, pattern
			}
		}
	}
	// Check output (e.g., Ansible output contains the actual error)
	for _, pattern := range TransientErrorPatterns {
		if strings.Contains(output, pattern) {
			return true, pattern
		}
	}
	return false, ""
}

// IsTransientErrorMsg checks if an error contains a transient error pattern.
func IsTransientErrorMsg(err error) (bool, string) {
	return IsTransientError(err, "")
}
