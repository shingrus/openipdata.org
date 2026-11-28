package main

// territoryOf is the one rule "a mainland label on a territory's own city": some layer rows (mostly
// operators' geofeeds) write Hong Kong or Macao as country CN. ISO 3166 and GeoNames list these
// territories under their own codes HK and MO, so with CN the builder writes the wrong country and
// cannot place the city (GeoNames has no "Hong Kong" in CN). A row whose country and normalised
// city (normCity: trimmed, lower case) are in the table below is treated in every respect as if the
// feed had written the territory's code. A row without a city is never changed.
//
// The table is deliberately NOT general: "a city name that exists under another country" is wrong
// as a rule (measured false friends: US/Riga, IT/Prague, FR/Cologne, US/Montreal). A new pair is
// added only after it has been measured. The layer files of 2026-11-25 hold no other spelling of
// the two territories under CN (rows: `Hong Kong` 983, `HONG KONG` 32, `Macao` 15), so the three
// names are complete for today's data; the real GeoNames list places `Macao` (an alternate name of
// Macau).
var territories = map[string]map[string]string{
	"CN": {
		"hong kong": "HK",
		"macao":     "MO",
		"macau":     "MO",
	},
}

// territoryOf returns the territory's code for a row with the (upper-case) country code and the
// raw city, and whether the row moved. Both the build (buildGeoNames) and the pre-pass of the
// full-list fallback (wantedFromLayers) call it, so they see the same country for a row.
func territoryOf(code, city string) (string, bool) {
	if to, ok := territories[code][normCity(city)]; ok {
		return to, true
	}
	return code, false
}
