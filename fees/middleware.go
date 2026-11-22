package fees

import (
	"encore.dev/beta/errs"
	"encore.dev/middleware"
	"encore.dev/rlog"
)

// HideInternalErrors keeps the cause of a server-side failure out of the
// response. Only errors the caller can act on keep their message; anything
// else is logged in full and answered with its code and a fixed message.
//
//encore:middleware target=all
func (s *Service) HideInternalErrors(req middleware.Request, next middleware.Next) middleware.Response {
	resp := next(req)
	if resp.Err == nil {
		return resp
	}

	code := errs.Code(resp.Err)
	switch code {
	case errs.InvalidArgument, errs.NotFound, errs.FailedPrecondition:
		return resp
	}

	rlog.Error("handle request", "err", resp.Err)
	message := "internal error"
	if code == errs.Unavailable {
		message = "temporarily unavailable, retry shortly"
	}
	resp.Err = &errs.Error{Code: code, Message: message}
	return resp
}
