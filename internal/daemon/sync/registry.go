package sync

import (
	"database/sql"
	"fmt"
)

// NotespaceBinding is display/root metadata for an immutable local identity.
// It is never used as a routing key.
type NotespaceBinding struct {
	ID      string `json:"notespace_id"`
	Name    string `json:"notespace_name"`
	Root    string `json:"root"`
	Subject string `json:"subject"`
	Kind    string `json:"kind"`
}

func (d *DB) UpsertNotespaceBinding(b NotespaceBinding) error {
	_, err := d.db.Exec(`INSERT INTO sync_notespaces(notespace_id,notespace_name,root,subject,kind)
		VALUES(?,?,?,?,?) ON CONFLICT(notespace_id) DO UPDATE SET
		notespace_name=excluded.notespace_name,root=excluded.root,subject=excluded.subject,kind=excluded.kind`,
		b.ID, b.Name, b.Root, b.Subject, b.Kind)
	if err != nil {
		return fmt.Errorf("record notespace binding %s: %w", b.ID, err)
	}
	return nil
}

func (d *DB) GetNotespaceBinding(id string) (*NotespaceBinding, error) {
	var b NotespaceBinding
	err := d.db.QueryRow(`SELECT notespace_id,notespace_name,root,subject,kind FROM sync_notespaces WHERE notespace_id=?`, id).
		Scan(&b.ID, &b.Name, &b.Root, &b.Subject, &b.Kind)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &b, nil
}
