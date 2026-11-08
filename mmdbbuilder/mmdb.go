package main

// Helpers shared by the MMDB builds.

import (
	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
)

func newWriter(dbType, description string) (*mmdbwriter.Tree, error) {
	return mmdbwriter.New(mmdbwriter.Options{
		DatabaseType:            dbType,
		Description:             map[string]string{"en": description},
		RecordSize:              28,
		IPVersion:               6,
		IncludeReservedNetworks: true,
		DisableIPv4Aliasing:     true,
	})
}

func setString(m mmdbtype.Map, key, val string) {
	if val == "" {
		return
	}
	m[mmdbtype.String(key)] = mmdbtype.String(val)
}
