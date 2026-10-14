package core

import "fmt"

type Error struct {
	Code    string
	Message string
	Status  int
	Details any
}

func (e *Error) Error() string { return e.Message }
func NewError(code, message string, status int) *Error {
	return &Error{Code: code, Message: message, Status: status}
}
func InvalidLink(message string) *Error { return NewError("invalid_link", message, 400) }
func Upstream(message string) *Error    { return NewError("upstream_extraction_error", message, 502) }
func MissingAsset() *Error {
	return NewError("asset_not_found_or_expired", "Asset has expired or is unavailable", 404)
}
func MissingExtraction() *Error {
	return NewError("extraction_not_found_or_expired", "Extraction has expired or is unavailable", 404)
}
func ConfigError(key string) error { return fmt.Errorf("invalid configuration: %s", key) }
