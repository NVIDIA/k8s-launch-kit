#!/bin/bash
# Copyright 2026 NVIDIA CORPORATION & AFFILIATES
# SPDX-License-Identifier: Apache-2.0
# Fixed adapter for digest-pinned netshoot tools. No credentials, eval or repairs.
set -euo pipefail
umask 077
request=$1
node=$2
mode=$3
for tool in jq curl ip getent timeout date sort awk; do command -v "$tool" >/dev/null; done
[[ "$mode" == pod-network || "$mode" == node-network ]]
[[ -n "$node" && -n "$POD_UID" ]]
# Bound all commands, including DNS and network inspection, by the CLI deadline.
if [[ ${4:-} != bounded ]]; then
    deadline=$(jq -r '.deadline | sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601' "$request")
    budget=$((deadline - $(date +%s)))
    ((budget > 0 && budget <= 3600))
    exec timeout "$budget" /bin/bash "$0" "$request" "$node" "$mode" bounded
fi
ulimit -f 8192
started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
per_timeout=$(jq -r '.requestTimeout / 1000000000' "$request")
: > /scratch/results.jsonl
while IFS= read -r destination; do
    host=$(jq -r '.hostname' <<<"$destination")
    port=$(jq -r '.port' <<<"$destination")
    verify_name=$(jq -r '.verifyName' <<<"$destination")
    authority=$(jq -r '.authority' <<<"$destination")
    url=$(jq -r '.url' <<<"$destination")
    ca=$(jq -r '.caFile' <<<"$destination")
    dns=resolved
    if [[ $(jq -r '.literal' <<<"$destination") == true ]]; then
        dns=not-applicable
        addresses=$(jq -cn --arg host "$host" '[$host]')
    else
        # STREAM rows cover both families, deduplicated without a happy-eyeballs fallback.
        # getent's lookup failure remains explicit unavailable evidence.
        status=0
        timeout "$per_timeout" getent ahosts "$host" > /scratch/dns || status=$?
        case "$status" in 0|2|124) ;; *) exit 1 ;; esac
        if ((status != 0)); then
            dns=unavailable
            addresses='[]'
        else
            addresses=$(awk '$2 == "STREAM" { print $1 }' /scratch/dns | sort -u | jq -Rsc 'split("\n") | map(select(length > 0))')
            if [[ $(jq 'length' <<<"$addresses") == 0 ]]; then dns=unavailable; fi
        fi
    fi
    (($(jq 'length' <<<"$addresses") <= 32))
    : > /scratch/addresses.jsonl
    while IFS= read -r address; do
        resolved=$address
        if [[ "$resolved" == *:* ]]; then resolved="[$resolved]"; fi
        name=$verify_name
        if [[ "$name" == *:* ]]; then name="[$name]"; fi
        ca_args=()
        if [[ -n "$ca" ]]; then ca_args=(--cacert "$ca"); fi
        # Disable curlrc, proxies, redirects and all non-HTTPS protocols. Each IP
        # is pinned while retaining the original HTTP authority and TLS identity.
        status=0
        curl --disable --silent --show-error --noproxy '*' --proxy '' \
            --proto '=https' --proto-redir '=https' --tlsv1.2 --max-redirs 0 \
            --connect-timeout "$per_timeout" --max-time "$per_timeout" \
            --max-filesize 65536 --retry 0 --output /scratch/body \
            --write-out '%{json}' --resolve "$name:$port:$resolved" \
            --header "Host: $authority" "${ca_args[@]}" --url "$url" \
            > /scratch/curl.json 2>/scratch/curl.stderr || status=$?
        # A tool/configuration failure must not masquerade as a failed network path.
        case "$status" in 0|5|6|7|18|28|35|52|55|56|60|63|80|92) ;; *) exit 1 ;; esac
        jq -e --argjson status "$status" '.exitcode == $status and (.time_connect | type == "number") and (.time_appconnect | type == "number")' /scratch/curl.json >/dev/null
        tcp=unavailable
        tls=unavailable
        http=http-unavailable
        if jq -e '.time_connect > 0' /scratch/curl.json >/dev/null; then tcp=reachable; fi
        if jq -e '.time_appconnect > 0 and .ssl_verify_result == 0' /scratch/curl.json >/dev/null; then tls=verified; fi
        code=$(jq '.response_code' /scratch/curl.json)
        if [[ "$tls" == verified ]]; then
            if ((code == 401 || code == 403)); then http=authentication-required
            elif ((code >= 300 && code < 400)); then http=redirect-refused
            elif ((code == 404)); then http=resource-unavailable
            elif ((code >= 400)); then http=application-error
            elif ((code >= 200 && code < 300)); then
                http=invalid-response
                if ((status == 0)) && jq -se 'length == 1 and (.[0] | type == "object")' /scratch/body >/dev/null 2>&1; then http=observed; fi
            fi
        fi
        jq -cn --arg address "$address" --arg tcp "$tcp" --arg tls "$tls" --arg http "$http" --argjson code "$code" \
            '{address:$address,tcp:$tcp,tls:$tls,http:$http,httpStatus:$code}' >> /scratch/addresses.jsonl
    done < <(jq -r '.[]' <<<"$addresses")
    jq -cn --arg node "$node" --arg mode "$mode" --arg dns "$dns" \
        --arg id "$(jq -r '.id' <<<"$destination")" --slurpfile addresses /scratch/addresses.jsonl \
        '{nodeUID:$node,networkMode:$mode,destination:$id,dns:$dns,addresses:$addresses,evidenceRef:""}' >> /scratch/results.jsonl
done < <(jq -c '.destinations[]' "$request")
printf '[]' > /scratch/interfaces.json
printf '[]' > /scratch/routes.json
if [[ "$mode" == node-network ]]; then
    ip -j -N address show > /scratch/links.json
    ip -j -N -4 route show table all > /scratch/routes4.json
    ip -j -N -6 route show table all > /scratch/routes6.json
    jq '[.[] | {name:.ifname,index:.ifindex,mtu:.mtu,addresses:[.addr_info[] | "\(.local)/\(.prefixlen)"]}]' /scratch/links.json > /scratch/interfaces.json
    jq -n --slurpfile links /scratch/links.json --slurpfile r4 /scratch/routes4.json --slurpfile r6 /scratch/routes6.json \
        '[($r4[0]+$r6[0])[] | . as $r | {destination:(.dst // "default"),gateway:(.gateway // ""),table:((.table // 254)|tonumber),interfaceIndex:([$links[0][] | select(.ifname == $r.dev) | .ifindex][0] // 0)}]' > /scratch/routes.json
fi
jq -cn --arg started "$started" --arg finished "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --arg node "$node" --arg pod "$POD_UID" --arg mode "$mode" --slurpfile request "$request" \
    --slurpfile results /scratch/results.jsonl --slurpfile interfaces /scratch/interfaces.json --slurpfile routes /scratch/routes.json \
    '{startedAt:$started,finishedAt:$finished,version:$request[0].version,runID:$request[0].runID,nodeUID:$node,podUID:$pod,networkMode:$mode,results:$results,interfaces:$interfaces[0],routes:$routes[0]}'
