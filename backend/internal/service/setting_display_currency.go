package service

import (
	"context"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	SiteDisplayCurrencyUSD = "USD"
	SiteDisplayCurrencyCNY = "CNY"
)

// ValidateSiteDisplayCurrency validates an explicit write to the display setting.
func ValidateSiteDisplayCurrency(currency string) error {
	switch currency {
	case SiteDisplayCurrencyUSD, SiteDisplayCurrencyCNY:
		return nil
	default:
		return infraerrors.BadRequest("INVALID_SITE_DISPLAY_CURRENCY", "site_display_currency must be USD or CNY")
	}
}

func parseSiteDisplayCurrency(value string) string {
	if value == SiteDisplayCurrencyCNY {
		return SiteDisplayCurrencyCNY
	}
	return SiteDisplayCurrencyUSD
}

func readSiteDisplayCurrency(ctx context.Context, repo SettingRepository) string {
	if repo != nil {
		if value, err := repo.GetValue(ctx, SettingKeySiteDisplayCurrency); err == nil {
			return parseSiteDisplayCurrency(value)
		}
	}
	return SiteDisplayCurrencyUSD
}

// siteDisplayCurrencySymbol labels site credits without changing their numeric value.
func siteDisplayCurrencySymbol(currency string) string {
	if currency == SiteDisplayCurrencyCNY {
		return "¥"
	}
	return "$"
}
