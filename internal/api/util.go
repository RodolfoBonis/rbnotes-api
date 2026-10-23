package api

import "github.com/jackc/pgx/v5/pgtype"

// parseUUIDParam converte uma string em pgtype.UUID (erro se malformada).
func parseUUIDParam(s string) (pgtype.UUID, error) {
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		return pgtype.UUID{}, err
	}
	return u, nil
}
