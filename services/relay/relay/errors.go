package relay

import "errors"

var (
	errUnauthorized = errors.New("unauthorized")
	errInvalidCode  = errors.New("invalid or expired connect code")
	errGatewayGone  = errors.New("gateway not available")
)
