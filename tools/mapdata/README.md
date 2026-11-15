# mapdata

Makes `site/static/world.bin`, the country outlines the website draws its map from. Standard library only; run by hand, never by the worker or the site build. The generated file is committed (like the fonts); the input is not.

## Source

- Natural Earth 1:50m "Admin 0 - Countries", GeoJSON: `https://raw.githubusercontent.com/nvkelso/natural-earth-vector/master/geojson/ne_50m_admin_0_countries.geojson`
- Licence: public domain (naturalearthdata.com/about/terms-of-use).
- Input file used: 3,083,490 bytes, sha256 `3e458fc036ad0a66411f2c1e6cac49c5d7bfb81cb1123bc513b22511a2b7fdeb`. The tool prints the sha256 of the input it read, so you can compare.
- Natural Earth draws disputed borders by its own (de facto) rules. The owner chose country borders knowingly.

## Regenerate

    curl -L -o /tmp/countries50.geojson https://raw.githubusercontent.com/nvkelso/natural-earth-vector/master/geojson/ne_50m_admin_0_countries.geojson
    cd tools/mapdata && go run . -in /tmp/countries50.geojson -out ../../site/static/world.bin

Defaults: `-tol 0.08` (Douglas-Peucker tolerance in degrees) and `-minarea 0.02` (smallest ring kept, in square degrees; a country always keeps its largest ring, so small island states such as Singapore, Malta, Hong Kong and Bahrain stay).

## What it keeps

Per entry: the ISO 3166-1 alpha-2 code (`ISO_A2_EH`, which is right for France, Norway and Kosovo; an entry with no valid code, such as Somaliland, is kept as land without a code; when two entries share a code, the one whose own `ADM0_A3` is that country keeps it), the label point (`LABEL_X`, `LABEL_Y`), the bounding box and the outer rings of its polygons (holes are dropped), counter-clockwise, in steps of 0.01 degree. The exact format is the comment at the top of `main.go` (the reader is `site/world.go`).

## Tolerance and size

The map frame is 46 x 18.2 degrees drawn about 500 px wide, so 0.08 degree is about one pixel.

| `-tol` | `world.bin` |
|--------|-------------|
| 0.05   | 65,089 bytes |
| 0.08 (used) | 51,064 bytes |
| 0.12   | 41,541 bytes |

Test: `go vet ./... && go test -race ./...`.
