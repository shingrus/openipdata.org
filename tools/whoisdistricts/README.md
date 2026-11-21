# whoisdistricts

Makes `hatchet/geofeeds/whois-districts.txt`, the list the whois text layer (`37-whois-text`) uses to drop a match on a small place that is most likely a city district. Standard library only; run by hand, never by the worker. The generated file is committed; the inputs are not.

## Why

`descr: DALSTON` on a Virgin Media inetnum means Dalston in London, but GeoNames `cities1000` has no districts (feature code PPLX), so the layer named Dalston in Cumbria, a village of 1,285 people 400 km away. Measured 2026-11-21: when the chosen place has fewer than 15,000 people and its name is also a district near a city of 100,000 or more of the same country, stronger layers agree with the village 14.5% of the time (44% for other small places).

## Source

- GeoNames `allCountries.zip` and `cities1000.zip`, `https://download.geonames.org/export/dump/`.
- Licence: CC BY 4.0 (https://www.geonames.org/). The credit is in the header of the generated file and in `LICENSE-DATA`.

## Regenerate

    curl -L -o /tmp/allCountries.zip https://download.geonames.org/export/dump/allCountries.zip
    curl -L -o /tmp/cities1000.zip https://download.geonames.org/export/dump/cities1000.zip
    cd tools/whoisdistricts && go run . -all /tmp/allCountries.zip -cities /tmp/cities1000.zip -out ../../hatchet/geofeeds/whois-districts.txt

`allCountries.zip` is about 420 MB (1.6 GB unzipped); the tool streams it and never unzips it to disk. `-date YYYY-MM-DD` sets the download date written in the header (default: the modified date of `-cities`).

## What it keeps

A line `CC<TAB>name` for the name and the asciiname of every PPLX feature that lies within 30 km (haversine) of a place of 100,000 people or more of the same country (from `cities1000`) and whose name, after a loose fold (lower case, accents removed, `st` as `saint`, letters a-z only), equals the name or asciiname of a place of fewer than 15,000 people of that country in `cities1000`. Sorted, distinct, deterministic. The fold is over-inclusive: the worker looks the names up again with its own normalisation (`districtSet` in `hatchet/geofeeds/whoisdistricts.go`).

Test: `go vet ./... && go test -race ./...`.
