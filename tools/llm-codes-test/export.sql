-- Router PTR names (hops only, never the traced targets) of the last 180 days, and the learned rules.
\copy (SELECT DISTINCT lower(n.ptr) FROM public.ip_ptr_names n JOIN (SELECT DISTINCT unnest(hop_ips) AS ip FROM public.network_traces WHERE traced_at > now() - interval '180 days') h ON h.ip = n.ip WHERE n.status = 'ok' AND n.ptr IS NOT NULL) TO 'router-names.txt'
\copy (SELECT domain, rank, status, places::text FROM public.rdns_city_rules WHERE status <> 'rejected') TO 'learned-rules.tsv'
