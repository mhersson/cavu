package store

import "time"

// PushSubscription is a browser's Web Push subscription, its keys base64url as the browser gave them.
type PushSubscription struct {
	ID                             int64
	Endpoint, P256DH, Auth, Origin string
	CreatedAt                      time.Time
	LastOKAt                       *time.Time
}
