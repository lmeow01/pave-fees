package fees

import (
	"fmt"
	"strings"
)

const (
	defaultPageSize = 100
	maxPageSize     = 1000
)

// pageSize resolves the page_size query parameter, where zero means it was
// not given.
func pageSize(requested int) (int, error) {
	switch {
	case requested == 0:
		return defaultPageSize, nil
	case requested < 0 || requested > maxPageSize:
		return 0, invalidArgument(fmt.Sprintf("page_size must be between 1 and %d", maxPageSize))
	}
	return requested, nil
}

// validatePageToken accepts an empty token (the first page) or the ID of the
// last item on the previous page, which is what next_page_token carries.
func validatePageToken(token, idPrefix string) error {
	if token == "" {
		return nil
	}
	if !strings.HasPrefix(token, idPrefix+"_") || validateText("next_page_token", token, maxKeyLength) != nil {
		return invalidArgument("next_page_token is not valid")
	}
	return nil
}

// splitPage takes rows fetched with a limit of size+1. The extra row only
// proves that another page exists; it is dropped, and the ID of the last row
// kept becomes the token for the next page.
func splitPage[T any](rows []T, size int, id func(T) string) (page []T, nextPageToken string) {
	if len(rows) <= size {
		return rows, ""
	}
	return rows[:size], id(rows[size-1])
}
