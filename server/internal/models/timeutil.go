package models

import "time"

// Timestamps are stored as RFC3339 UTC strings rather than relying on the
// SQLite driver's own time.Time marshalling, so the on-disk format is
// explicit and stable regardless of driver version.

func toDB(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

func toDBPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return toDB(*t)
}

func fromDB(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func fromDBPtr(s *string) *time.Time {
	if s == nil || *s == "" {
		return nil
	}
	t := fromDB(*s)
	return &t
}
