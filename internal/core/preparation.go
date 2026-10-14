package core

import "time"

type IdentityPreparation struct {
	State      string     `json:"state"`
	Proxies    int        `json:"proxies"`
	Target     int        `json:"target"`
	Ready      int        `json:"ready"`
	Failed     int        `json:"failed"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

type IdentityPreparer interface {
	StartPreparation() (IdentityPreparation, error)
	PreparationStatus() IdentityPreparation
}
