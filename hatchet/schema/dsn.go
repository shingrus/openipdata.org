package schema

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// ErrInvalidConfig replaces connection-string parse errors.
var ErrInvalidConfig = errors.New("invalid PGSQL configuration")

// HideDSN returns ErrInvalidConfig for a PGSQL parse error and err otherwise.
// pgx redacts the password only when it recognizes the connection-string
// format; otherwise its parse error echoes the whole string, and Hatchet
// stores the errors tasks return. Use it on every error from pgx.Connect,
// pgx.ParseConfig, or pgxpool.ParseConfig.
func HideDSN(err error) error {
	var parseErr *pgconn.ParseConfigError
	if errors.As(err, &parseErr) {
		return ErrInvalidConfig
	}
	return err
}
