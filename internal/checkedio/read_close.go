// Package checkedio preserves errors when closing resources owned by readers.
package checkedio

import (
	"errors"
	"fmt"
	"io"
)

// CloseReadResult joins a reader's close error into its named error result.
// Use it only when the caller owns the resource and has not closed it already.
// It is not appropriate for reporting consensus execution or committed writes.
func CloseReadResult(resource io.Closer, result *error) {
	if err := resource.Close(); err != nil {
		*result = errors.Join(*result, fmt.Errorf("close read resource: %w", err))
	}
}
