package methods

import (
	"github.com/velocitykode/velocity-mcp/server"
)

// validationMessage renders a validation error into a single client-facing
// string. It delegates to the server package so every handler, and the tool
// catalog's inner calls, report a validation failure with identical wording.
func validationMessage(err error) string {
	return server.ValidationMessage(err)
}
