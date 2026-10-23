package panel

import (
	"errors"
	"time"
)

// Sponsor is one paid placement published in the repo's sponsors.json.
type Sponsor struct {
	ID     string            `json:"id" example:"acme-2026-10"`
	Name   string            `json:"name" example:"Acme VPS"`
	Enable *bool             `json:"enable,omitempty" example:"true"`
	Slots  []string          `json:"slots"`
	From   *time.Time        `json:"from,omitempty" example:"2026-10-01T00:00:00Z"`
	Until  time.Time         `json:"until" example:"2026-11-01T00:00:00Z"`
	Logo   string            `json:"logo,omitempty" example:"/sponsors/logo/acme.png"`
	Title  map[string]string `json:"title"`
	Text   map[string]string `json:"text"`
	Link   string            `json:"link" example:"https://acme.example/?utm_source=3x-ui"`
}

// SponsorList is the active sponsor set plus the contact link for new sponsors.
type SponsorList struct {
	Contact  string    `json:"contact,omitempty" example:"https://t.me/example"`
	Sponsors []Sponsor `json:"sponsors"`
}

// ErrSponsorLogoUnknown rejects logo names not used by an active sponsor.
var ErrSponsorLogoUnknown = errors.New("unknown sponsor logo")

// GetSponsors returns the currently active sponsors. Built-in ads are disabled.
func (s *PanelService) GetSponsors() (*SponsorList, error) {
	return &SponsorList{Sponsors: []Sponsor{}}, nil
}

// GetSponsorLogo returns the image bytes for a logo of a currently active sponsor.
func (s *PanelService) GetSponsorLogo(name string) ([]byte, string, error) {
	return nil, "", ErrSponsorLogoUnknown
}
