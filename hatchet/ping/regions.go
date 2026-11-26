package ping

// region is a Google Cloud region the pings run from and the site name of its column in
// hosting-rtt.csv.gz: where Google's data centre of that region is (<CC>/<City>, as GeoNames
// names the place). The order is the order of the columns.
type region struct{ Name, Site string }

var regions = []region{
	{"us-east1", "US/Moncks Corner"},
	{"us-east4", "US/Ashburn"},
	{"us-central1", "US/Council Bluffs"},
	{"us-south1", "US/Dallas"},
	{"us-west1", "US/The Dalles"},
	{"us-west2", "US/Los Angeles"},
	{"northamerica-northeast1", "CA/Montréal"},
	{"southamerica-east1", "BR/Osasco"},
	{"southamerica-west1", "CL/Santiago"},
	{"europe-west1", "BE/Saint-Ghislain"},
	{"europe-west2", "GB/London"},
	{"europe-west3", "DE/Frankfurt am Main"},
	{"europe-west4", "NL/Delfzijl"},
	{"europe-north1", "FI/Hamina"},
	{"europe-southwest1", "ES/Madrid"},
	{"europe-central2", "PL/Warsaw"},
	{"me-west1", "IL/Tel Aviv"},
	{"me-central1", "QA/Doha"},
	{"africa-south1", "ZA/Johannesburg"},
	{"asia-south1", "IN/Mumbai"},
	{"asia-southeast1", "SG/Singapore"},
	{"asia-southeast2", "ID/Jakarta"},
	{"asia-east1", "TW/Changhua"},
	{"asia-east2", "HK/Hong Kong"},
	{"asia-northeast1", "JP/Tokyo"},
	{"asia-northeast3", "KR/Seoul"},
	{"australia-southeast1", "AU/Sydney"},
}
