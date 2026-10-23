package panel

import (
	"testing"
)

func TestGetSponsorsEmpty(t *testing.T) {
	svc := &PanelService{}
	got, err := svc.GetSponsors()
	if err != nil {
		t.Fatalf("GetSponsors() error = %v", err)
	}
	if got == nil || len(got.Sponsors) != 0 {
		t.Fatalf("expected 0 sponsors, got %v", got)
	}
}

func TestGetSponsorLogoDisabled(t *testing.T) {
	svc := &PanelService{}
	_, _, err := svc.GetSponsorLogo("test.png")
	if err != ErrSponsorLogoUnknown {
		t.Fatalf("expected ErrSponsorLogoUnknown, got %v", err)
	}
}
