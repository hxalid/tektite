package common

import (
	"github.com/google/uuid"
	"github.com/spirit-labs/tektite/errors"
	log "github.com/spirit-labs/tektite/logger"
)

func LogInternalError(err error) errors.TektiteError {
	id, err2 := uuid.NewRandom()
	var errRef string
	if err2 != nil {
		log.Errorf("failed to generate uuid %v", err)
		errRef = ""
	} else {
		errRef = id.String()
	}
	// For internal errors we don't return internal error messages to the CLI as this would leak
	// server implementation details. Instead, we generate a random UUID and add that to the message
	// and log the internal error in the server logs with the UUID so it can be looked up
	perr := errors.NewInternalError(errRef)
	log.Errorf("internal error (reference %s) occurred %+v", errRef, err)
	return perr
}
