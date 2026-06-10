package methods

import (
	"errors"
	"sort"
	"strings"

	"github.com/velocitykode/velocity/contract"
)

// validationMessage renders a validation error into a single client-facing
// string, mirroring laravel/mcp's Support\ValidationMessages::from (flatten all
// field messages, join with a space). Field iteration order is sorted so the
// message is deterministic. When no field messages can be recovered, a generic
// fallback is returned so no internal detail leaks.
func validationMessage(err error) string {
	var verr contract.ValidationErrors
	if errors.As(err, &verr) && len(verr.Errors) > 0 {
		fields := make([]string, 0, len(verr.Errors))
		for field := range verr.Errors {
			fields = append(fields, field)
		}
		sort.Strings(fields)

		var msgs []string
		for _, field := range fields {
			msgs = append(msgs, verr.Errors[field]...)
		}
		if len(msgs) > 0 {
			return strings.Join(msgs, " ")
		}
	}
	return "The given data was invalid."
}
