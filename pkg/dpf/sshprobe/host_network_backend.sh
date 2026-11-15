#!/bin/bash
# Copyright 2026 NVIDIA CORPORATION & AFFILIATES
# SPDX-License-Identifier: Apache-2.0
# Fixed protocol implementation. No eval, user commands, apply, or networkd reload.
set -Eeuo pipefail
backend_pid=$BASHPID
export LC_ALL=C
export PATH=/usr/sbin:/usr/bin:/sbin:/bin
umask 077
action=${1:?}
encoded=${2:?}
if ! command -v jq >/dev/null; then printf '%s\n' '{"error":"HOST_NETWORK_TOOL_MISSING_JQ"}'; exit 0; fi
if ! command -v base64 >/dev/null; then printf '%s\n' '{"error":"HOST_NETWORK_TOOL_MISSING_BASE64"}'; exit 0; fi
shopt -s inherit_errexit
request=$(printf '%s' "$encoded" | base64 -d)
key=$(jq -er '.key | select(test("^[0-9a-f]{24}$"))' <<<"$request")
iface=$(jq -er '.request.identity.interfaceName' <<<"$request")
pci=$(jq -er '.request.identity.pciAddress | ascii_downcase' <<<"$request")
connection=$(jq -er '.connection' <<<"$request")
read -r client_ip _client_port server_ip _server_port <<<"$connection"
[[ "$iface" =~ ^[a-zA-Z0-9_.:-]{1,15}$ && "$iface" != -* && "$iface" != . && "$iface" != .. ]]
[[ "$pci" =~ ^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$ ]]
directory=/var/lib/l8k-host-network
journal=$directory/$key.json
fragment=/etc/netplan/99-l8k-workload-$key.yaml
request_id=$(jq -r '.requestID // ""' <<<"$request")
created=0 changed_mtu=0 added_address=0 added_route=0 journal_created=0 fragment_removed=0 fragment_expected=0
fragment_hash='' journal_hash='' fragment_temp='' shadow='' shadow_inode='' planned_artifacts='' old_mtu=0 phase=preflight live_generated=0 failure_reason=HOST_NETWORK_OPERATION_ROLLED_BACK artifacts_before=''

receipt() {
    jq -cn --arg requestID "$request_id" --arg state "$1" --arg reason "$2" --arg journal "$journal" --arg fragment "$fragment" --arg hash "$fragment_hash" \
      --argjson old "$old_mtu" --argjson mtu "$changed_mtu" --argjson address "$added_address" --argjson route "$added_route" --argjson blocking "$3" \
      '{requestID:$requestID,state:$state,reasonCode:$reason,journalPath:$journal,fragmentPath:$fragment,fragmentSHA256:$hash,oldMTU:$old,changedMTU:($mtu==1),addedAddress:($address==1),addedRoute:($route==1),runtimeVerified:false,fileVerified:false,blocking:($blocking==1)}'
}
die() {
    trap - ERR
    if [[ "$action" == snapshot ]]; then jq -cn --arg error "$1" '{error:$error}'
    elif [[ "$journal_created" == 1 || "$1" == HOST_NETWORK_PRIOR_INTENT_BLOCKS || "$1" == HOST_NETWORK_OWNERSHIP_CONFLICT ]]; then receipt partial "$1" 1
    else receipt refused "$1" 0; fi
    exit 0
}
# Child substitutions/pipelines must propagate failure, never compensate and
# return a successful receipt into data consumed by the owning transaction.
trap 'status=$?; [[ $BASHPID == "$backend_pid" ]] || exit "$status"; die HOST_NETWORK_PREFLIGHT_FAILED' ERR
[[ "$EUID" == 0 ]] || die HOST_NETWORK_ROOT_REQUIRED
for tool in jq ip ethtool dmidecode lspci readlink netplan networkctl systemctl dpkg-query sha256sum sync flock mktemp ln stat base64 awk cat tr head grep mkdir mv rm cp chown getent find sort od; do
    command -v "$tool" >/dev/null || { tool_code=${tool//-/_}; die "HOST_NETWORK_TOOL_MISSING_${tool_code^^}"; }
done
[[ -x /usr/libexec/netplan/generate && -f /usr/libexec/netplan/generate && ! -L /usr/libexec/netplan/generate ]] || die HOST_NETWORK_TOOL_MISSING_NETPLAN_GENERATOR
network_gid=$(getent group systemd-network | awk -F: '$1=="systemd-network" && $3~/^[0-9]+$/{print $3}') || die HOST_NETWORK_ACCOUNT_UNAVAILABLE
root_gid=$(getent group root | awk -F: '$1=="root" && $3~/^[0-9]+$/{print $3}') || die HOST_NETWORK_ACCOUNT_UNAVAILABLE
[[ "$network_gid" =~ ^[0-9]+$ && "$root_gid" == 0 ]] || die HOST_NETWORK_ACCOUNT_UNAVAILABLE

# A matching value is not a successful observation when its producer failed.
value_is() {
    local expected=$1 observed
    shift
    observed=$("$@") || return 1
    [[ "$observed" == "$expected" ]]
}
# All snapshot reads return status and set an explicit caller-owned destination.
# They never emit protocol JSON or exit: snapshot owns refusal, while configure
# must return through its transaction handler before reporting failed readback.
capture_command() {
    local read_step=$1 read_tool=$2 read_destination=$3 read_output read_status
    shift 3
    if read_output=$("$@" 2>/dev/null); then
        printf -v "$read_destination" '%s' "$read_output"
    else
        read_status=$?
        capture_failure_code="HOST_NETWORK_READ_${read_step}_${read_tool}_EXIT_${read_status}"
        return 1
    fi
}
# Vendor1.1.2 enumerates live interface names, but reads matching MAC/driver
# facts below root-dir/sys. Keep the complete matching input private and stable.
read_generator_context() {
    local context_destination=$1 context_entry context_name context_mac context_driver context_driver_present context_status
    local context_data='' context_before context_after
    local -a context_entries context_recheck
    [[ -d /sys/class/net && ! -L /sys/class/net ]] || { capture_failure_code=HOST_NETWORK_GENERATOR_CONTEXT_UNSUPPORTED; return 1; }
    context_entries=()
    for context_entry in /sys/class/net/* /sys/class/net/.[!.]* /sys/class/net/..?*; do
        [[ -d "$context_entry" ]] || continue # Exclude class attributes such as bonding_masters.
        context_entries+=("$context_entry")
        (( ${#context_entries[@]} <= 1024 )) || { capture_failure_code=HOST_NETWORK_GENERATOR_CONTEXT_UNSUPPORTED; return 1; }
    done
    (( ${#context_entries[@]} > 0 )) || { capture_failure_code=HOST_NETWORK_GENERATOR_CONTEXT_UNSUPPORTED; return 1; }
    printf -v context_before '%s\n' "${context_entries[@]}"
    for context_entry in "${context_entries[@]}"; do
        context_name=${context_entry##*/}
        # Preserve Linux names, including punctuation/UTF8 bytes, but no path,
        # internal-record separator, whitespace or control characters.
        [[ ${#context_name} -gt 0 && ${#context_name} -le 15 && "$context_name" != . && "$context_name" != .. && ! "$context_name" =~ [[:cntrl:][:space:]/:] ]] || { capture_failure_code=HOST_NETWORK_GENERATOR_CONTEXT_UNSUPPORTED; return 1; }
        # sysfs address is at most20 bytes formatted as hex; bound the read
        # before buffering. Empty addresses on non-Ethernet devices are valid.
        if { IFS= read -r -n 128 context_mac < "$context_entry/address"; } 2>/dev/null; then :; else
            context_status=$?
            capture_failure_code="HOST_NETWORK_READ_GENERATOR_CONTEXT_BASH_EXIT_${context_status}"; return 1
        fi
        [[ -z "$context_mac" || "$context_mac" =~ ^([[:xdigit:]]{2}:){0,19}[[:xdigit:]]{2}$ ]] || { capture_failure_code=HOST_NETWORK_GENERATOR_CONTEXT_UNSUPPORTED; return 1; }
        [[ -n "$context_mac" ]] || context_mac=-
        context_driver=- context_driver_present=0
        if [[ -L "$context_entry/device/driver" ]]; then
            # readlink's kernel-provided target is PATH_MAX bounded. Only its
            # checked basename is copied, never an absolute live sysfs target.
            capture_command GENERATOR_CONTEXT READLINK context_driver readlink -- "$context_entry/device/driver" || return 1
            context_driver=${context_driver##*/}
            [[ "$context_driver" =~ ^[A-Za-z0-9_.+-]{1,64}$ && "$context_driver" != . && "$context_driver" != .. ]] || { capture_failure_code=HOST_NETWORK_GENERATOR_CONTEXT_UNSUPPORTED; return 1; }
            context_driver_present=1
        elif [[ -e "$context_entry/device/driver" ]]; then
            capture_failure_code=HOST_NETWORK_GENERATOR_CONTEXT_UNSUPPORTED; return 1
        fi
        context_data+="$context_name"$'\t'"$context_mac"$'\t'"$context_driver_present"$'\t'"$context_driver"$'\n'
    done
    context_recheck=()
    for context_entry in /sys/class/net/* /sys/class/net/.[!.]* /sys/class/net/..?*; do
        [[ -d "$context_entry" ]] || continue
        context_recheck+=("$context_entry")
        (( ${#context_recheck[@]} <= 1024 )) || { capture_failure_code=HOST_NETWORK_GENERATOR_CONTEXT_CHANGED; return 1; }
    done
    printf -v context_after '%s\n' "${context_recheck[@]}"
    [[ "$context_before" == "$context_after" ]] || { capture_failure_code=HOST_NETWORK_GENERATOR_CONTEXT_CHANGED; return 1; }
    printf -v "$context_destination" '%s' "$context_data"
}

write_generator_context() {
    local context_root=$1 context_catalog=$2 context_name context_mac context_driver_present context_driver
    while IFS=$'\t' read -r context_name context_mac context_driver_present context_driver; do
        [[ -n "$context_name" ]] || continue
        mkdir -p -- "$context_root/sys/class/net/$context_name" || return 1
        [[ "$context_mac" != - ]] || context_mac=''
        printf '%s\n' "$context_mac" >"$context_root/sys/class/net/$context_name/address" || return 1
        if [[ "$context_driver_present" == 1 ]]; then
            mkdir -p -- "$context_root/sys/class/net/$context_name/device" || return 1
            ln -s -- "$context_driver" "$context_root/sys/class/net/$context_name/device/driver" || return 1
        fi
    done <<<"$context_catalog"
}

generator_context_fence() {
    local current_context
    read_generator_context current_context || { failure_reason=$capture_failure_code; return 1; }
    [[ "$current_context" == "$generator_context" ]] || { failure_reason=HOST_NETWORK_GENERATOR_CONTEXT_CHANGED; return 1; }
}

secure_file() {
    [[ -f "$1" && ! -L "$1" ]] && value_is 0:0:600:1 stat -c '%u:%g:%a:%h' -- "$1"
}
protected_source() {
    local owner mode metadata
    metadata=$(stat -c '%u %a' -- "$1") || return 1
    read -r owner mode <<<"$metadata" || return 1
    [[ "$owner" == 0 && "$mode" =~ ^[0-7]{3,4}$ ]] || return 1
    (( (8#$mode & 8#22) == 0 ))
}
source_digest() {
    local root=${1:-} relative d f digest size count=0 total=0
    {
        for relative in /lib/netplan /etc/netplan /run/netplan; do
            d=$root$relative
            [[ ! -L "$d" ]] || return 1
            [[ -d "$d" ]] || continue
            protected_source "$d" || return 1
            for f in "$d"/*.yaml; do
                [[ -e "$f" || -L "$f" ]] || continue
                [[ "$f" == "$root$fragment" ]] && continue
                [[ -f "$f" && ! -L "$f" && ${f##*/} =~ ^[A-Za-z0-9_.-]+\.yaml$ ]] && protected_source "$f" || return 1
                ((count+=1)); ((count<=256)) || return 1
                size=$(stat -c %s -- "$f") || return 1
                total=$((total+size)); ((total<=1048576)) || return 1
                digest=$(sha256sum -- "$f"|awk '{print $1}') || return 1
                printf '%s  %s\n' "$digest" "${f#"$root"}"
            done
        done
    } | sha256sum | awk '{print $1}'
}

# Only these qualified output locations may be emitted by this backend version.
# Other runtime systemd trees are neither copied nor traversed recursively.
artifact_paths() {
    local root=${1:-} dir f relative found count=0
    for relative in /run /run/systemd /run/udev /run/NetworkManager; do
        dir=$root$relative
        [[ -e "$dir" || -L "$dir" ]] || continue
        [[ -d "$dir" && ! -L "$dir" ]] && protected_source "$dir" || return 1
    done
    for relative in /run/systemd/network /run/NetworkManager/conf.d /run/NetworkManager/system-connections /run/udev/rules.d /run/systemd/system/systemd-networkd-wait-online.service.d; do
        dir=$root$relative
        [[ -e "$dir" || -L "$dir" ]] || continue
        [[ -d "$dir" && ! -L "$dir" ]] && protected_source "$dir" || return 1
        for f in "$dir"/*; do
            [[ -e "$f" || -L "$f" ]] || continue
            ((count+=1)); ((count<=1024)) || return 1
            [[ "$f" =~ ^[A-Za-z0-9_./:@+-]+$ ]] || return 1
            printf '%s\n' "$f"
        done
    done
    for relative in /run/systemd/system /run/systemd/system/systemd-networkd.service.wants; do
        dir=$root$relative
        [[ -e "$dir" || -L "$dir" ]] || continue
        [[ -d "$dir" && ! -L "$dir" ]] && protected_source "$dir" || return 1
    done
    # Unknown Netplan-owned units/links cannot be hidden by the narrow manifest.
    found=''
    if [[ -d "$root/run/systemd/system" ]]; then
        found=$(find -P "$root/run/systemd/system" -mindepth 1 -maxdepth 2 -name 'netplan-*' -printf '%p\n') || return 1
    fi
    while IFS= read -r f; do
        [[ -n "$f" ]] || continue
        ((count+=1)); ((count<=1024)) || return 1
        case "${f#"$root"}" in
            /run/systemd/system/netplan-ovs-cleanup.service|/run/systemd/system/systemd-networkd.service.wants/netplan-ovs-cleanup.service) ;;
            *) return 1;;
        esac
    done <<<"$found"
    for relative in /run/systemd/system/netplan-ovs-cleanup.service /run/systemd/system/systemd-networkd.service.wants/netplan-ovs-cleanup.service; do
        f=$root$relative
        [[ -e "$f" || -L "$f" ]] || continue
        printf '%s\n' "$f"
    done
}

# Only the selected member of the qualified native ANY-routable command is
# owned here. The same bounded byte image is validated and projected; raw text
# never passes through Bash substitution/read (which could drop NUL or LFs).
wait_online_content() {
    local wait_root=$1 wait_mode=$2 wait_required=${3:-0}
    local wait_file=$wait_root/run/systemd/system/systemd-networkd-wait-online.service.d/10-netplan.conf
    local wait_parent wait_relative
    for wait_relative in /run /run/systemd /run/systemd/system /run/systemd/system/systemd-networkd-wait-online.service.d; do
        wait_parent=$wait_root$wait_relative
        [[ -d "$wait_parent" && ! -L "$wait_parent" ]] && protected_source "$wait_parent" || return 1
    done
    [[ -f "$wait_file" && ! -L "$wait_file" ]] && protected_source "$wait_file" && value_is 1 stat -c %h -- "$wait_file" || return 1
    [[ "$wait_mode" == project || "$wait_mode" == owned || "$wait_mode" == inspect ]] || return 1
    od -An -v -tu1 -N65537 -- "$wait_file" | awk -v iface="$iface" -v mode="$wait_mode" -v required="$wait_required" '
      {for(i=1;i<=NF;i++){n++;b=$i+0;last=b;if(n>65536 || (b<32 && b!=10) || b==127 || b>255){bad=1}else if(!bad){data=data sprintf("%c",b)}}}
      END{
        if(bad||n==0||last!=10)exit 1
        exe="ExecStart=/lib/systemd/systemd-networkd-wait-online"
        route=exe " --any -o routable"
        count=split(data,lines,"\n")
        for(line_no=1;line_no<count;line_no++){
          line=lines[line_no];skip=0;known=0;trimmed=line
          sub(/^[ ]+/, "", trimmed);sub(/[ ]+$/, "", trimmed)
          if(substr(trimmed,length(trimmed),1)=="\\")exit 1
          if(substr(trimmed,1,1)=="["){
            if(line=="[Unit]"){if(unit_seen++||service_seen)exit 1;section="unit";known=1}
            else if(line=="[Service]"){if(!unit_seen||service_seen++)exit 1;section="service";known=1}
            else {unknown_owned=1;section="unknown"}
          }
          if(line=="ConditionPathIsSymbolicLink=/run/systemd/generator/network-online.target.wants/systemd-networkd-wait-online.service"){
            if(section!="unit"||condition_seen++)exit 1;known=1
          }
          if(index(trimmed,"ExecStart")==1){
            if(line!=trimmed||section!="service")exit 1
            if(line=="ExecStart="){if(reset_seen++||commands)exit 1;known=1}
            else if(index(line,exe)!=1)exit 1
          }
          if(index(line,exe)==1){
            if(section!="service"||!reset_seen||index(line,"\\"))exit 1
            commands++;known=1
            if(index(line,route " ")==1){kind="route";prefix=route;if(route_seen++)exit 1}
            else if(index(line,exe " ")==1){kind="link";prefix=exe;if(link_seen++)exit 1}
            else exit 1
            rest=substr(line,length(prefix)+2);tokens=split(rest,args," ");joined="";kept=""
            if(tokens<2||tokens%2)exit 1
            for(j=1;j<=tokens;j++){joined=joined (j>1?" ":"") args[j]}
            if(joined!=rest)exit 1
            for(j=1;j<=tokens;j+=2){
              if(args[j]!="-i"||args[j+1]=="")exit 1
              name=args[j+1]
              if(kind=="link"){if(name!~ /:(degraded|carrier)$/)exit 1;sub(/:(degraded|carrier)$/, "", name)}
              if(length(name)==0||length(name)>15||name=="."||name==".."||name~ /[[:space:]\/:]/)exit 1
              key=kind SUBSEP name;if(seen[key]++)exit 1
              if(name==iface){if(kind=="route"){selected_route++;continue}else selected_link++}
              kept=kept " -i " args[j+1]
            }
            if(kind=="route"&&selected_route){if(kept=="")skip=1;else line=prefix kept}
          }
          if(!known&&trimmed!=""&&trimmed!~ /^[#;]/)unknown_owned=1
          if(!skip)output=output line "\n"
        }
        if(service_seen&&!reset_seen)exit 1
        if(mode!="project"&& (unknown_owned||unit_seen!=1||condition_seen!=1))exit 1
        if(mode=="owned"&& (selected_route!=required||selected_link!=required))exit 1
        if(mode=="project")printf "%s",output
      }'
}

# Complete current-MAC facts are private; a direct selected-name match proves
# its uniquely bound vendor set-name key even if another name shares the MAC.
selected_wait_online_match() {
    local match_destination=$1 match_catalog=$2 match_expected=${3,,}
    local match_name match_mac match_driver_present match_driver
    local match_selected=0 match_direct=0 match_other=0
    while IFS=$'\t' read -r match_name match_mac match_driver_present match_driver; do
        [[ -n "$match_name" ]] || continue
        if [[ "$match_name" == "$iface" ]]; then
            match_selected=1
            [[ "${match_mac,,}" != "$match_expected" ]] || match_direct=1
        elif [[ "${match_mac,,}" == "$match_expected" ]]; then match_other=1; fi
    done <<<"$match_catalog"
    [[ "$match_selected" == 1 ]] || return 1
    if [[ "$match_direct" == 1 ]]; then printf -v "$match_destination" '%s' direct
    elif [[ "$match_other" == 1 ]]; then printf -v "$match_destination" '%s' other-only
    else printf -v "$match_destination" '%s' absent; fi
}

selected_wait_online_requirement() {
    local wait_destination=$1 wait_match
    selected_wait_online_match wait_match "$generator_context" "$expected_mac" || return 1
    if [[ "$wait_online_optional" == true || "$wait_match" == absent ]]; then printf -v "$wait_destination" '%s' 0
    elif [[ "$wait_match" == direct ]]; then printf -v "$wait_destination" '%s' 1
    else return 1; fi # Cannot infer unrelated matching-definition precedence.
}

artifact_record() {
    local root=$1 f=$2 comparison=${3:-all} relative metadata digest target
    relative=${f#"$root"}
    metadata=$(stat -c '%u:%g:%a:%h' -- "$f") || return 1
    if [[ -L "$f" ]]; then
        [[ "$relative" == /run/systemd/system/systemd-networkd.service.wants/netplan-ovs-cleanup.service && "$metadata" == 0:0:777:1 ]] || return 1
        target=$(readlink -- "$f") || return 1
        [[ "$target" == /run/systemd/system/netplan-ovs-cleanup.service ]] || return 1
        printf 'L\t%s\t%s\t%s\n' "$relative" "$metadata" "$target"
    else
        [[ -f "$f" ]] && value_is 1 stat -c %h -- "$f" && protected_source "$f" || return 1
        if [[ "$comparison" == nonselected && "$relative" == /run/systemd/system/systemd-networkd-wait-online.service.d/10-netplan.conf ]]; then
            digest=$(wait_online_content "$root" project | sha256sum | awk '{print $1}') || return 1
        else digest=$(sha256sum -- "$f"|awk '{print $1}') || return 1; fi
        printf 'F\t%s\t%s\t%s\n' "$relative" "$metadata" "$digest"
    fi
}

artifact_digest() {
    local root=${1:-} mode=${2:-all} paths f relative size total=0
    paths=$(artifact_paths "$root") || return 1
    {
        while IFS= read -r f; do
            [[ -n "$f" ]] || continue
            relative=${f#"$root"}
            if [[ "$mode" == nonselected && ( "$relative" == "/run/systemd/network/10-netplan-$logical.network" || "$relative" == "/run/systemd/network/10-netplan-$logical.link" ) ]]; then continue; fi
            if [[ ! -L "$f" ]]; then
                size=$(stat -c %s -- "$f") || return 1
                ((size<=1048576)) || return 1
                total=$((total+size)); ((total<=16777216)) || return 1
            fi
            artifact_record "$root" "$f" "$mode" || return 1
        done <<<"$paths"
    } | sort | sha256sum | awk '{print $1}'
}

cleanup_shadow() {
    [[ -n "$shadow" ]] || return 0
    [[ "$shadow" == "$directory"/.render-* && -d "$shadow" && ! -L "$shadow" ]] && value_is "0:700:$shadow_inode" stat -c '%u:%a:%d:%i' "$shadow" || return 1
    rm -rf -- "$shadow" || return 1
    sync -- "$directory" || return 1
    shadow=''
    shadow_inode=''
}

validate_shadow_tree() {
    local kind relative count=0
    find "$shadow/run" -mindepth 1 -printf '%y\t%P\n' >"$shadow/.outputs" || return 1
    while IFS=$'\t' read -r kind relative; do
        ((count+=1)); ((count<=2048)) || return 1
        [[ "$relative" =~ ^[A-Za-z0-9_./:@+-]+$ ]] || return 1
        if [[ "$kind" == d ]]; then
            case "$relative" in
                netplan|systemd|systemd/network|systemd/system|systemd/system/systemd-networkd.service.wants|systemd/system/systemd-networkd-wait-online.service.d|NetworkManager|NetworkManager/conf.d|NetworkManager/system-connections|udev|udev/rules.d) ;;
                *) return 1;;
            esac
        elif [[ "$kind" == f && "$relative" == netplan/*.yaml && "${relative#netplan/}" != */* ]]; then
            : # Copied source YAML; the source manifest is independently fenced.
        else
            case "$relative" in
                systemd/network/*|NetworkManager/conf.d/*|NetworkManager/system-connections/*|udev/rules.d/*|systemd/system/systemd-networkd-wait-online.service.d/*|systemd/system/netplan-ovs-cleanup.service|systemd/system/systemd-networkd.service.wants/netplan-ovs-cleanup.service)
                    artifact_record "$shadow" "$shadow/run/$relative" >/dev/null || return 1;;
                *) return 1;;
            esac
        fi
    done <"$shadow/.outputs"
}

prepare_shadow() {
    local relative source f paths copied_sources observed_artifacts
    shadow=$(mktemp -d "$directory/.render-XXXXXX") || return 1
    shadow_inode=$(stat -c '%d:%i' "$shadow") || return 1
    write_journal intent '' || return 1
    for relative in /lib/netplan /etc/netplan /run/netplan; do
        [[ -d "$relative" ]] || continue
        mkdir -p "$shadow$relative" || return 1
        for source in "$relative"/*.yaml; do
            [[ -e "$source" || -L "$source" ]] || continue
            [[ -f "$source" && ! -L "$source" ]] && protected_source "$source" || return 1
            cp --preserve=mode,ownership -- "$source" "$shadow$source" || return 1
        done
    done
    copied_sources=$(source_digest "$shadow") || return 1
    [[ "$copied_sources" == "$expected_sources" ]] || return 1
    paths=$(artifact_paths '') || return 1
    while IFS= read -r f; do
        [[ -n "$f" ]] || continue
        artifact_record '' "$f" >/dev/null || return 1
        [[ ! -L "$f" ]] || continue # The vendor creates the one qualified link.
        mkdir -p "$shadow${f%/*}" || return 1
        failure_reason=HOST_NETWORK_GENERATED_METADATA_UNSUPPORTED
        cp --preserve=mode,ownership -- "$f" "$shadow$f" || return 1
        failure_reason=HOST_NETWORK_SHADOW_UNAVAILABLE
    done <<<"$paths"
    if [[ -n "$payload" ]]; then
        printf '# l8k-host-network-v1 %s\n%s\n' "$request_id" "$payload" >"$shadow$fragment" || return 1
    fi
    # Reproduce every vendor-consumed matching fact in private regular files
    # and basename-only relative links. Never expose live /sys to the shadow.
    generator_context_fence || return 1
    write_generator_context "$shadow" "$generator_context" || return 1
    # Use the qualified vendor generation context; private intent/source creation stays077.
    (trap - ERR; umask 022; /usr/libexec/netplan/generate --root-dir "$shadow") >/dev/null || return 1
    generator_context_fence || return 1
    validate_shadow_tree || return 1
    copied_sources=$(source_digest "$shadow") || return 1
    [[ "$copied_sources" == "$expected_sources" ]] || return 1
    failure_reason=HOST_NETWORK_GENERATED_METADATA_UNSUPPORTED
    verify_generated_files "$shadow" || return 1
    failure_reason=HOST_NETWORK_WAIT_ONLINE_CONTRIBUTION_INVALID
    wait_online_content "$shadow" owned "$wait_online_required" || return 1
    failure_reason=HOST_NETWORK_GENERATED_OUTPUT_CONFLICT
    observed_artifacts=$(artifact_digest "$shadow" nonselected) || return 1
    [[ "$observed_artifacts" == "$generated_before" ]] || return 1
    planned_artifacts=$(artifact_digest "$shadow" all) || return 1
    observed_artifacts=$(artifact_digest '' all) || return 1
    [[ "$observed_artifacts" == "$artifacts_before" ]] || return 1
    fence || return 1
    cleanup_shadow || return 1
    write_journal intent '' || return 1
}
capture() {
    local destination=$1 context_destination=${2:-} context_catalog context_digest
    local uuid boot vpd binding port vf=0 permanent link addresses routes rules4 rules6 defaults4 defaults6 management_route management_device management_addresses network_file merged source source_before source_final os nv sv iv nd nm nm_status fingerprint runtime_digest canonical observed_binding result nm_properties
    capture_failure_code=''
    capture_command IDENTITY DMIDECODE uuid dmidecode -s system-uuid || return 1
    uuid=${uuid,,}
    capture_command BOOT_ID CAT boot cat /proc/sys/kernel/random/boot_id || return 1
    capture_command PCI_VPD LSPCI vpd lspci -D -vv -s "$pci" || return 1
    capture_command PCI_VPD BASE64 vpd base64 -w0 <<<"$vpd" || return 1
    capture_command PCI_BINDING READLINK binding readlink -f -- "/sys/class/net/$iface/device" || return 1
    capture_command PHYSICAL_PORT CAT port cat "/sys/class/net/$iface/phys_port_name" || return 1
    [[ ! -L "/sys/class/net/$iface/device/physfn" ]] || vf=1
    capture_command PERMANENT_MAC ETHTOOL permanent ethtool -P "$iface" || return 1
    capture_command PCI_BINDING_RECHECK READLINK observed_binding readlink -f -- "/sys/class/net/$iface/device" || return 1
    [[ "$binding" == "$observed_binding" ]] || { capture_failure_code=HOST_NETWORK_READ_PCI_BINDING_CHANGED; return 1; }
    capture_command LINK IP link ip -j -d link show dev "$iface" || return 1
    capture_command ADDRESSES IP addresses ip -j -4 address show dev "$iface" || return 1
    capture_command ROUTES IP routes ip -j -4 route show table all || return 1
    capture_command RUNTIME_DIGEST JQ canonical jq -cnS --argjson addresses "$addresses" --argjson routes "$routes" --argjson link "$link" '{addresses:$addresses,routes:$routes,mtu:$link[0].mtu}' || return 1
    capture_command RUNTIME_DIGEST SHA256SUM runtime_digest sha256sum <<<"$canonical" || return 1
    runtime_digest=${runtime_digest%% *}
    capture_command RULES4 IP rules4 ip -j -4 rule show || return 1
    capture_command RULES6 IP rules6 ip -j -6 rule show || return 1
    capture_command DEFAULTS4 IP defaults4 ip -j -4 route show default table all || return 1
    capture_command DEFAULTS6 IP defaults6 ip -j -6 route show default table all || return 1
    capture_command MANAGEMENT_ROUTE IP management_route ip -j route get "$client_ip" from "$server_ip" || return 1
    capture_command MANAGEMENT_ROUTE JQ management_route jq -c 'map(del(.uid,.cache))' <<<"$management_route" || return 1
    capture_command MANAGEMENT_DEVICE JQ management_device jq -er 'select(length==1) | .[0].dev' <<<"$management_route" || return 1
    [[ "$management_device" =~ ^[a-zA-Z0-9_.:-]{1,15}$ && "$management_device" != "$iface" ]] || { capture_failure_code=HOST_NETWORK_READ_MANAGEMENT_DEVICE_INVALID; return 1; }
    capture_command MANAGEMENT_ADDRESSES IP management_addresses ip -j address show dev "$management_device" || return 1
    capture_command MANAGEMENT_ADDRESSES JQ management_addresses jq -c '[.[].addr_info[]|{family,local,prefixlen,scope,label}] | sort_by(.family,.local,.prefixlen)' <<<"$management_addresses" || return 1
    capture_command NETWORK_FILE NETWORKCTL network_file networkctl status --no-pager --no-legend "$iface" || return 1
    capture_command NETWORK_FILE AWK network_file awk '/Network File:/{sub(/^.*Network File:[[:space:]]*/, "");print}' <<<"$network_file" || return 1
    case "$network_file" in
        ''|n/a|-) capture_failure_code=HOST_NETWORK_READ_NETWORK_FILE_MISSING; return 1;;
    esac
    [[ -f "$network_file" && ! -L "$network_file" ]] || { capture_failure_code=HOST_NETWORK_READ_NETWORK_FILE_INVALID; return 1; }
    capture_command NETWORK_FILE_METADATA STAT canonical stat -c '%u %a' -- "$network_file" || return 1
    local owner mode
    read -r owner mode <<<"$canonical" || { capture_failure_code=HOST_NETWORK_READ_NETWORK_FILE_METADATA_INVALID; return 1; }
    [[ "$owner" == 0 && "$mode" =~ ^[0-7]{3,4}$ ]] && (( (8#$mode & 8#22) == 0 )) || { capture_failure_code=HOST_NETWORK_READ_NETWORK_FILE_METADATA_INVALID; return 1; }
    capture_command MANAGEMENT_ADDRESS JQ canonical jq -e --arg address "$server_ip" 'any(.[];.local==$address)' <<<"$management_addresses" || return 1
    # Bind Go's merged view to the same protected source bytes copied/fenced by
    # the transaction. No snapshot/context output is published until the final
    # source check also covers all later live reads in this capture.
    capture_command NETPLAN_SOURCE SOURCE_DIGEST source_before source_digest || return 1
    capture_command NETPLAN_MERGED NETPLAN merged netplan get || return 1
    capture_command NETPLAN_MERGED BASE64 merged base64 -w0 <<<"$merged" || return 1
    capture_command NETPLAN_SOURCE SOURCE_DIGEST source source_digest || return 1
    [[ "$source" == "$source_before" ]] || { capture_failure_code=HOST_NETWORK_SOURCE_CHANGED; return 1; }
    capture_command OS_RELEASE AWK os awk -F= '/^ID=/{gsub(/"/,"",$2);id=$2}/^VERSION_ID=/{gsub(/"/,"",$2);v=$2}END{print id ":" v}' /etc/os-release || return 1
    capture_command NETPLAN_VERSION DPKG_QUERY nv dpkg-query -W '-f=${Version}' netplan.io || return 1
    capture_command NETWORKD_VERSION NETWORKCTL sv networkctl --version || return 1
    sv=${sv%%$'\n'*}
    capture_command IP_VERSION IP iv ip -Version || return 1
    capture_command NETWORKD_STATE SYSTEMCTL nd systemctl is-active systemd-networkd || return 1
    if nm=$(systemctl is-active NetworkManager 2>/dev/null); then
        [[ "$nm" == active ]] || { capture_failure_code=HOST_NETWORK_READ_NETWORKMANAGER_STATE_INVALID; return 1; }
    else
        nm_status=$?
        if [[ "$nm_status" == 3 && "$nm" == inactive ]]; then :
        elif [[ "$nm_status" == 4 && "$nm" == inactive ]]; then
            # is-active uses exit4 for an absent unit on the qualified systemd.
            # Absence is acceptable only with successful, exact inactive/dead
            # not-found properties; arbitrary status4 never proves no manager.
            capture_command NETWORKMANAGER_PROPERTIES SYSTEMCTL nm_properties systemctl show NetworkManager.service --property=LoadState,ActiveState,SubState || return 1
            [[ "$nm_properties" == $'LoadState=not-found\nActiveState=inactive\nSubState=dead' ]] || { capture_failure_code=HOST_NETWORK_READ_NETWORKMANAGER_ABSENCE_UNCONFIRMED; return 1; }
        else capture_failure_code="HOST_NETWORK_READ_NETWORKMANAGER_STATE_SYSTEMCTL_EXIT_${nm_status}"; return 1; fi
    fi
    read_generator_context context_catalog || return 1
    local context_match context_other_only=false
    selected_wait_online_match context_match "$context_catalog" "${permanent#Permanent address: }" || { capture_failure_code=HOST_NETWORK_GENERATOR_CONTEXT_CHANGED; return 1; }
    [[ "$context_match" != other-only ]] || context_other_only=true
    # Read-only discovery rejects known unsupported unit shape/ancestry without
    # generating files or assuming the post-lease selected membership.
    wait_online_content '' inspect || { capture_failure_code=HOST_NETWORK_WAIT_ONLINE_UNSUPPORTED; return 1; }
    capture_command GENERATOR_CONTEXT SHA256SUM context_digest sha256sum <<<"$context_catalog" || return 1
    context_digest=${context_digest%% *}
    # Stable identity/source/management fence deliberately excludes the selected
    # runtime fields this action is explicitly permitted to change.
    capture_command FENCE JQ canonical jq -cnS --arg uuid "$uuid" --arg boot "$boot" --arg vpd "$vpd" --arg binding "$binding" --arg port "$port" --arg permanent "$permanent" \
      --arg source "$source" --arg context "$context_digest" --arg os "$os" --arg nv "$nv" --arg sv "$sv" --arg iv "$iv" --arg nd "$nd" --arg nm "$nm" --arg nf "$network_file" \
      --argjson link "$link" --argjson mr "$management_route" --argjson ma "$management_addresses" --argjson r4 "$rules4" --argjson r6 "$rules6" \
      --argjson d4 "$defaults4" --argjson d6 "$defaults6" \
      '{uuid:$uuid,boot:$boot,vpd:$vpd,binding:$binding,port:$port,permanent:$permanent,source:$source,context:$context,os:$os,nv:$nv,sv:$sv,iv:$iv,nd:$nd,nm:$nm,networkFile:$nf,link:($link|map(del(.mtu))),managementRoute:$mr,managementAddresses:$ma,rules4:$r4,rules6:$r6,defaults4:$d4,defaults6:$d6}' || return 1
    capture_command FENCE SHA256SUM fingerprint sha256sum <<<"$canonical" || return 1
    fingerprint=${fingerprint%% *}
    capture_command SNAPSHOT_JSON JQ result jq -cn --arg os "$os" --arg nv "$nv" --arg sv "$sv" --arg iv "$iv" --arg nd "$nd" --arg nm "$nm" --arg uuid "$uuid" --arg boot "$boot" \
      --arg vpd "$vpd" --arg binding "$binding" --arg port "$port" --arg permanent "$permanent" --argjson vf "$vf" \
      --argjson link "$link" --argjson addresses "$addresses" --argjson routes "$routes" --argjson r4 "$rules4" --argjson r6 "$rules6" --argjson d6 "$defaults6" \
      --argjson mr "$management_route" --argjson ma "$management_addresses" --arg md "$management_device" --arg nf "$network_file" --rawfile merged /dev/stdin --arg source "$source" --arg fence "$fingerprint" --arg runtime "$runtime_digest" --argjson otherOnly "$context_other_only" \
      '{waitOnlineOtherMatchOnly:$otherOnly,os:$os,netplanVersion:$nv,systemdVersion:$sv,ipVersion:$iv,networkd:$nd,otherManager:$nm,uuid:$uuid,bootID:$boot,vpd:$vpd,binding:$binding,physicalPort:$port,vf:($vf==1),permanentMAC:$permanent,link:$link,addresses:$addresses,routes:$routes,rules4:$r4,rules6:$r6,defaultRoutes6:$d6,managementRoute:$mr,managementAddresses:$ma,managementDevice:$md,networkFile:$nf,merged:$merged,sourceDigest:$source,fence:$fence,runtimeDigest:$runtime}' <<<"$merged" || return 1
    capture_command NETPLAN_SOURCE SOURCE_DIGEST source_final source_digest || return 1
    [[ "$source_final" == "$source" ]] || { capture_failure_code=HOST_NETWORK_SOURCE_CHANGED; return 1; }
    printf -v "$destination" '%s' "$result"
    if [[ -n "$context_destination" ]]; then printf -v "$context_destination" '%s' "$context_catalog"; fi
    return 0
}
if [[ "$action" == snapshot ]]; then
    if capture snapshot; then printf '%s\n' "$snapshot"; else die "$capture_failure_code"; fi
    exit 0
fi
[[ "$action" == configure ]] || die HOST_NETWORK_ACTION_INVALID
[[ "$request_id" =~ ^[0-9a-f]{64}$ ]] || die HOST_NETWORK_INPUT_INVALID
jq -e 'has("waitOnlineOptional") and (.waitOnlineOptional|type=="boolean")' <<<"$request" >/dev/null || die HOST_NETWORK_INPUT_INVALID
wait_online_optional=$(jq -r '.waitOnlineOptional' <<<"$request") || die HOST_NETWORK_INPUT_INVALID
logical=$(jq -er '.logicalID|select(test("^[A-Za-z0-9_-]{1,63}$"))' <<<"$request")
host_prefix=$(jq -er '.request.hostAddressPrefix' <<<"$request")
host_ip=${host_prefix%/*}
workload=$(jq -er '.request.workloadCIDR' <<<"$request")
gateway=$(jq -er '.request.gateway' <<<"$request")
probe_address=$(jq -er '.probeAddress' <<<"$request")
desired_mtu=$(jq -er '.request.mtu' <<<"$request")
old_mtu=$(jq -er '.oldMTU' <<<"$request")
expected_fence=$(jq -er '.fence' <<<"$request")
expected_mac=$(jq -er '.request.identity.permanentMAC|ascii_downcase' <<<"$request")
payload=$(jq -er '.fragment' <<<"$request" | base64 -d)
[[ -d /etc/netplan && ! -L /etc/netplan ]] || die HOST_NETWORK_SOURCE_INVALID
if [[ ! -d /var/lib || -L /var/lib ]] || ! protected_source /var/lib; then die HOST_NETWORK_OWNERSHIP_CONFLICT; fi
if [[ ! -e "$directory" ]]; then mkdir -m700 "$directory"; sync -- /var/lib; fi
if [[ ! -d "$directory" || -L "$directory" ]] || ! value_is 0:700 stat -c '%u:%a' "$directory"; then die HOST_NETWORK_OWNERSHIP_CONFLICT; fi
exec 9<"$directory"
flock -x -w 5 9 || die HOST_NETWORK_LOCK_BUSY
directory_inode=$(stat -c '%d:%i' "$directory")
directory_guard() {
    [[ -d "$directory" && ! -L "$directory" ]] &&
        value_is "0:700:$directory_inode" stat -c '%u:%a:%d:%i' "$directory" &&
        value_is "$directory_inode" stat -Lc '%d:%i' /proc/self/fd/9
}
directory_guard || die HOST_NETWORK_OWNERSHIP_CONFLICT
if capture snapshot generator_context; then :; else die "$capture_failure_code"; fi
value_is "$expected_fence" jq -er '.fence' <<<"$snapshot" || die HOST_NETWORK_SOURCE_CHANGED
expected_runtime=$(jq -er '.runtimeDigest' <<<"$request") || die HOST_NETWORK_INPUT_INVALID
value_is "$expected_runtime" jq -er '.runtimeDigest' <<<"$snapshot" || die HOST_NETWORK_RUNTIME_CHANGED
prior_address_count=$(jq -er 'if .addressExists then 1 else 0 end' <<<"$request") || die HOST_NETWORK_INPUT_INVALID
prior_route_count=$(jq -er 'if .routeExists then 1 else 0 end' <<<"$request") || die HOST_NETWORK_INPUT_INVALID
expected_sources=$(jq -er '.sourceDigest' <<<"$snapshot")
selected_wait_online_requirement wait_online_required || die HOST_NETWORK_WAIT_ONLINE_CONTEXT_UNSUPPORTED
wait_online_content '' project >/dev/null || die HOST_NETWORK_WAIT_ONLINE_UNSUPPORTED

address_count() { ip -j -4 address show dev "$iface" | jq --arg ip "$host_ip" --argjson prefix "${host_prefix#*/}" '[.[].addr_info[]|select(.family=="inet" and .local==$ip and .prefixlen==$prefix)]|length'; }
route_count() { ip -j -4 route show table all | jq --arg dst "$workload" --arg gateway "$gateway" --arg dev "$iface" '[.[]|select(.dst==$dst and .gateway==$gateway and .dev==$dev and ((.table//"main")=="main") and ((.type//"unicast")=="unicast"))]|length'; }
current_mtu() { ip -j -d link show dev "$iface" | jq -er 'select(length==1)|.[0].mtu'; }
address_digest() { ip -j -4 address show dev "$iface" | jq -cS '[.[].addr_info[]]' | sha256sum | awk '{print $1}'; }
route_digest() { ip -j -4 route show table all | jq -cS --arg dst "$workload" '[.[]|select(.dst==$dst and ((.table//"main")=="main"))]' | sha256sum | awk '{print $1}'; }
prior_address_digest=$(address_digest)
prior_route_digest=$(route_digest)
expected_address_digest=$prior_address_digest
expected_route_digest=$prior_route_digest
# The source manifest deliberately excludes this resource. Its separate owner
# state must be checked on both sides of live source reads, including absence.
fragment_guard() {
    local observed_fragment
    if [[ "$fragment_removed" == 1 || "$fragment_expected" == 0 ]]; then
        if [[ ! -e "$fragment" && ! -L "$fragment" ]]; then return 0; fi
    elif secure_file "$fragment" && observed_fragment=$(sha256sum "$fragment"|awk '{print $1}') && [[ "$observed_fragment" == "$fragment_hash" ]]; then
        return 0
    fi
    failure_reason=HOST_NETWORK_OWNERSHIP_CONFLICT
    return 1
}
fence() {
    local observed_artifacts observed_address observed_route observed_journal now observed_context
    directory_guard || return 1
    if [[ -n "$artifacts_before" ]]; then
        observed_artifacts=$(artifact_digest '' all) || return 1
        if [[ "$live_generated" == 1 ]]; then [[ "$observed_artifacts" == "$planned_artifacts" ]] || return 1
        else [[ "$observed_artifacts" == "$artifacts_before" ]] || return 1; fi
    fi
    observed_address=$(address_digest) || return 1
    observed_route=$(route_digest) || return 1
    [[ "$observed_address" == "$expected_address_digest" && "$observed_route" == "$expected_route_digest" ]] || return 1
    fragment_guard || return 1
    if [[ "$journal_created" == 1 ]]; then
        secure_file "$journal" || return 1
        observed_journal=$(sha256sum "$journal"|awk '{print $1}') || return 1
        [[ "$observed_journal" == "$journal_hash" ]] || return 1
    fi
    capture now observed_context || { failure_reason=$capture_failure_code; return 1; }
    fragment_guard || return 1
    [[ "$observed_context" == "$generator_context" ]] || { failure_reason=HOST_NETWORK_GENERATOR_CONTEXT_CHANGED; return 1; }
    value_is "$expected_fence" jq -er '.fence' <<<"$now"
}
desired_runtime() {
    value_is "$desired_mtu" current_mtu && value_is 1 address_count && value_is 1 route_count || return 1
    ip -j -4 route get "$gateway" from "$host_ip" | jq -e --arg dev "$iface" 'length==1 and .[0].dev==$dev' >/dev/null || return 1
    ip -j -4 route get "$probe_address" from "$host_ip" | jq -e --arg dev "$iface" --arg gateway "$gateway" 'length==1 and .[0].dev==$dev and .[0].gateway==$gateway' >/dev/null
}
outside_routes_digest() {
    ip -j -4 route show table all | jq -cS --arg iface "$iface" --arg ip "$host_ip" --arg prefix "$host_prefix" --arg workload "$workload" --arg gateway "$gateway" '
      def number: split(".")|reduce .[] as $n (0; . * 256 + ($n|tonumber));
      def text: . as $n|[($n/16777216|floor),($n/65536|floor)%256,($n/256|floor)%256,$n%256]|map(tostring)|join(".");
      ($prefix|split("/")) as $p | pow(2;32-($p[1]|tonumber)) as $size |
      (((($p[0]|number)/$size)|floor)*$size) as $start |
      (($start|text)+"/"+$p[1]) as $network | (($start+$size-1)|text) as $broadcast |
      map(select((.dev==$iface and .protocol=="kernel" and (.gateway==null) and
        (.dst==$network or .dst==$ip or .dst==($ip+"/32") or .dst==$broadcast or .dst==($broadcast+"/32")))|not)) |
      map(select((.dst==$workload and .dev==$iface and .gateway==$gateway and ((.table//"main")=="main"))|not)) |
      sort_by(.table,.dst,.dev,.gateway)' | sha256sum | awk '{print $1}'
}
outside_before=$(outside_routes_digest)
runtime_guard() {
    local observed_outside
    observed_outside=$(outside_routes_digest) || return 1
    [[ "$observed_outside" == "$outside_before" ]] || return 1
    local expected_mtu=$old_mtu expected_address expected_route
    [[ "$changed_mtu" == 0 ]] || expected_mtu=$desired_mtu
    expected_address=$prior_address_count
    expected_route=$prior_route_count
    [[ "$added_address" == 0 ]] || expected_address=1
    [[ "$added_route" == 0 ]] || expected_route=1
    value_is "$expected_mtu" current_mtu && value_is "$expected_address" address_count && value_is "$expected_route" route_count || return 1
    ip -j -4 address show dev "$iface" | jq -e --arg ip "$host_ip" --argjson prefix "${host_prefix#*/}" 'all(.[].addr_info[]; .family!="inet" or (.local==$ip and .prefixlen==$prefix))' >/dev/null
}
verify_generated_files() {
    local root=${1:-} file link
    file=$root/run/systemd/network/10-netplan-$logical.network
    link=$root/run/systemd/network/10-netplan-$logical.link
    [[ -f "$file" && ! -L "$file" ]] && value_is "0:$network_gid:640:1" stat -c '%u:%g:%a:%h' "$file" || return 1
    [[ -f "$link" && ! -L "$link" ]] && value_is "0:$root_gid:640:1" stat -c '%u:%g:%a:%h' "$link" || return 1
    awk -v name="$iface" -v mac="$expected_mac" -v mtu="$desired_mtu" -v address="$host_prefix" -v dst="$workload" -v gateway="$gateway" '
      function route(){if(d=="default"||d=="0.0.0.0/0"||d=="::/0")bad=1;if(d==dst){if(g!=gateway)bad=1;routes++}d="";g=""}
      /^\[/{if(section=="[Route]")route();section=$0;next}
      section=="[Match]"&&$0=="Name="name{names++}
      section=="[Match]"&&tolower($0)=="permanentmacaddress="mac{macs++}
      section=="[Link]"&&$0=="MTUBytes="mtu{mtus++}
      section=="[Network]"&&$0=="Address="address{addresses++}
      section=="[Route]"&&/^Destination=/{d=substr($0,13)}
      section=="[Route]"&&/^Gateway=/{g=substr($0,9)}
      END{if(section=="[Route]")route();exit(bad||names!=1||macs!=1||mtus!=1||addresses!=1||routes!=1)}' "$file" || return 1
    awk -v name="$iface" -v mac="$expected_mac" -v mtu="$desired_mtu" '
      /^\[/{section=$0;next}
      section=="[Match]"&&tolower($0)=="permanentmacaddress="mac{macs++}
      section=="[Link]"&&$0=="Name="name{names++}
      section=="[Link]"&&$0=="MTUBytes="mtu{mtus++}
      END{exit(macs!=1||names!=1||mtus!=1)}' "$link"

}

generate_live() {
    (trap - ERR; umask 022; netplan generate) >/dev/null
}

verify_generated() {
    verify_generated_files '' || return 1
    value_is "$desired_mtu" netplan get "network.ethernets.$logical.mtu" || return 1
    netplan get "network.ethernets.$logical.addresses" | grep -Fx -e "- $host_prefix" -e "- \"$host_prefix\"" >/dev/null || return 1
    wait_online_content '' owned "$wait_online_required" || { failure_reason=HOST_NETWORK_WAIT_ONLINE_CONTRIBUTION_INVALID; return 1; }
}

if [[ -e "$journal" || -L "$journal" ]]; then
    secure_file "$journal" || die HOST_NETWORK_OWNERSHIP_CONFLICT
    jq -e --arg id "$request_id" '.owner=="l8k-host-network-v1" and .requestID==$id and .state=="complete"' "$journal" >/dev/null || die HOST_NETWORK_PRIOR_INTENT_BLOCKS
    [[ -z "$payload" ]] || die HOST_NETWORK_PERSISTENCE_CHANGED
    fragment_hash=$(jq -r '.fragmentSHA256' "$journal")
    if [[ "$fragment_hash" != "" ]]; then
        fragment_expected=1
        secure_file "$fragment" || die HOST_NETWORK_OWNERSHIP_CONFLICT
        observed_fragment=$(sha256sum "$fragment"|awk '{print $1}') || die HOST_NETWORK_OWNERSHIP_CONFLICT
        [[ "$observed_fragment" == "$fragment_hash" ]] || die HOST_NETWORK_OWNERSHIP_CONFLICT
    fi
    failure_reason=HOST_NETWORK_READBACK_FAILED
    if ! desired_runtime || ! fence || ! verify_generated; then die "$failure_reason"; fi
    # Selected Netplan getters read the live source again; close that read window.
    if ! fence; then die "$failure_reason"; fi
    jq --argjson mtu "$desired_mtu" 'del(.owner,.leaseBinding,.identity,.phase,.desired,.prior,.shadow) + {oldMTU:$mtu,changedMTU:false,addedAddress:false,addedRoute:false,runtimeVerified:true,fileVerified:true,blocking:false,verifiedRepeat:true}' "$journal"
    exit 0
fi
[[ ! -e "$fragment" && ! -L "$fragment" ]] || die HOST_NETWORK_OWNERSHIP_CONFLICT
observed_mtu=$(current_mtu) || die HOST_NETWORK_READBACK_FAILED
observed_address_count=$(address_count) || die HOST_NETWORK_READBACK_FAILED
observed_route_count=$(route_count) || die HOST_NETWORK_READBACK_FAILED
if [[ -z "$payload" && "$observed_mtu" == "$desired_mtu" && "$observed_address_count" == 1 && "$observed_route_count" == 1 ]]; then
    failure_reason=HOST_NETWORK_READBACK_FAILED
    if ! desired_runtime || ! fence || ! verify_generated; then die "$failure_reason"; fi
    # Selected Netplan getters read the live source again; close that read window.
    if ! fence; then die "$failure_reason"; fi
    receipt verified-external '' 0 | jq '. + {runtimeVerified:true,fileVerified:true,externalVerified:true,journalPath:"",fragmentPath:""}'
    exit 0
fi
[[ "$observed_mtu" == "$old_mtu" && "$observed_address_count" == "$prior_address_count" && "$observed_route_count" == "$prior_route_count" ]] || die HOST_NETWORK_RUNTIME_CHANGED

write_journal() {
    local state=$1 reason=$2 temp observed_journal
    directory_guard || return 1
    temp=$(mktemp "$directory/.intent-XXXXXX") || return 1
    receipt "$state" "$reason" 1 | jq --arg lease "$(jq -r '.request.leaseBinding' <<<"$request")" --argjson identity "$(jq -c '.request.identity' <<<"$request")" \
      --arg phase "$phase" --argjson desired "$(jq -c '.request|{mtu,hostAddressPrefix,gateway,workloadCIDR}' <<<"$request")" \
      --argjson prior "$(jq -c --arg address "$prior_address_digest" --arg route "$prior_route_digest" '{mtu:.oldMTU,addressPresent:.addressExists,routePresent:.routeExists,fragmentAbsent:true,runtimeDigest,fence,addressDigest:$address,routeDigest:$route}' <<<"$request")" \
      --arg shadow "$shadow" --arg inode "$shadow_inode" --arg artifacts "$planned_artifacts" \
      '.+{owner:"l8k-host-network-v1",leaseBinding:$lease,identity:$identity,phase:$phase,desired:$desired,prior:$prior,shadow:{path:$shadow,inode:$inode,plannedArtifacts:$artifacts}} | if .state=="complete" then .+{runtimeVerified:true,fileVerified:true,blocking:false} else . end' >"$temp" || return 1
    chown "0:$root_gid" "$temp" || return 1
    sync -- "$temp" || return 1
    if [[ "$journal_created" == 0 ]]; then ln -- "$temp" "$journal" || return 1; journal_created=1; else
        secure_file "$journal" || return 1
        observed_journal=$(sha256sum "$journal"|awk '{print $1}') || return 1
        [[ "$observed_journal" == "$journal_hash" ]] && jq -e --arg id "$request_id" '.owner=="l8k-host-network-v1" and .requestID==$id' "$journal" >/dev/null || return 1
        mv -T -- "$temp" "$journal" || return 1
    fi
    rm -f -- "$temp" || return 1
    sync -- "$directory" || return 1
    journal_hash=$(sha256sum "$journal"|awk '{print $1}') || return 1
}
generated_before=$(artifact_digest '' nonselected)
artifacts_before=$(artifact_digest '' all)
compensate() {
    trap - ERR HUP INT TERM
    set +e
    local uncertain=0 observed_fragment observed_generated observed_outside
    if ! fence; then write_journal partial HOST_NETWORK_COMPENSATION_UNCERTAIN; receipt partial HOST_NETWORK_COMPENSATION_UNCERTAIN 1; exit 0; fi
    if [[ "$added_route" == 1 ]]; then
        if fence && value_is 1 route_count; then
            if ip -4 route del "$workload" via "$gateway" dev "$iface"; then expected_route_digest=$prior_route_digest; else uncertain=1; fi
        else uncertain=1; fi
    elif [[ "$phase" == route && "$prior_route_count" == 0 ]]; then value_is 0 route_count || uncertain=1; fi
    if [[ "$added_address" == 1 ]]; then
        if fence && value_is 1 address_count; then
            if ip -4 address del "$host_prefix" dev "$iface"; then expected_address_digest=$prior_address_digest; else uncertain=1; fi
        else uncertain=1; fi
    elif [[ "$phase" == address && "$prior_address_count" == 0 ]]; then value_is 0 address_count || uncertain=1; fi
    if [[ "$changed_mtu" == 1 ]]; then
        if fence && value_is "$desired_mtu" current_mtu; then ip link set dev "$iface" mtu "$old_mtu" || uncertain=1; else uncertain=1; fi
    elif [[ "$phase" == mtu ]]; then value_is "$old_mtu" current_mtu || uncertain=1; fi
    if [[ "$created" == 1 ]]; then
        if fence && secure_file "$fragment" && observed_fragment=$(sha256sum "$fragment"|awk '{print $1}') && [[ "$observed_fragment" == "$fragment_hash" ]]; then
            if rm -- "$fragment"; then fragment_removed=1; sync -- /etc/netplan && generate_live && live_generated=0 || uncertain=1
            else uncertain=1; fi
        else uncertain=1; fi
    fi
    if [[ -n "$fragment_temp" && -e "$fragment_temp" ]]; then
        if secure_file "$fragment_temp" && observed_fragment=$(sha256sum "$fragment_temp"|awk '{print $1}') && [[ -n "$fragment_hash" && "$observed_fragment" == "$fragment_hash" ]]; then
            rm -- "$fragment_temp" && sync -- /etc/netplan || uncertain=1
        else uncertain=1; fi
    fi
    cleanup_shadow || uncertain=1
    observed_generated=$(artifact_digest '' nonselected) && [[ "$observed_generated" == "$generated_before" ]] || uncertain=1
    observed_outside=$(outside_routes_digest) && [[ "$observed_outside" == "$outside_before" ]] || uncertain=1
    value_is "$old_mtu" current_mtu && value_is "$prior_address_count" address_count && value_is "$prior_route_count" route_count || uncertain=1
    fence || uncertain=1
    if [[ "$uncertain" == 0 ]]; then
        if write_journal rolled-back "$failure_reason"; then receipt rolled-back "$failure_reason" 1
        else receipt partial HOST_NETWORK_JOURNAL_WRITE_UNCERTAIN 1; fi
    else write_journal partial HOST_NETWORK_COMPENSATION_UNCERTAIN; receipt partial HOST_NETWORK_COMPENSATION_UNCERTAIN 1; fi
    exit 0
}
phase=intent
if ! write_journal intent ''; then die HOST_NETWORK_JOURNAL_UNAVAILABLE; fi
trap '[[ $BASHPID == "$backend_pid" ]] || exit 1; compensate' ERR
trap 'trap - ERR; write_journal partial HOST_NETWORK_INTERRUPTED || true; receipt partial HOST_NETWORK_INTERRUPTED 1; exit 0' HUP INT TERM
phase=shadow
failure_reason=HOST_NETWORK_SHADOW_UNAVAILABLE
prepare_shadow
failure_reason=HOST_NETWORK_OPERATION_ROLLED_BACK
if [[ -n "$payload" ]]; then
    phase=fragment
    fence
    fragment_temp=$(mktemp /etc/netplan/.l8k-workload-XXXXXX)
    printf '# l8k-host-network-v1 %s\n%s\n' "$request_id" "$payload" >"$fragment_temp"
    chown "0:$root_gid" "$fragment_temp"
    fragment_hash=$(sha256sum "$fragment_temp"|awk '{print $1}')
    sync -- "$fragment_temp"
    ln -- "$fragment_temp" "$fragment"
    created=1
    fragment_expected=1
    rm -- "$fragment_temp"
    fragment_temp=''
    sync -- /etc/netplan
    write_journal intent ''
fi
phase=generate
fence
generate_live
live_generated=1
fence
verify_generated
observed_generated=$(artifact_digest '' nonselected)
[[ "$observed_generated" == "$generated_before" ]]
if [[ "$old_mtu" != "$desired_mtu" ]]; then
    phase=mtu; write_journal intent ''; fence; runtime_guard
    value_is "$old_mtu" current_mtu
    ip link set dev "$iface" mtu "$desired_mtu"
    changed_mtu=1; write_journal intent ''
fi
if [[ "$prior_address_count" == 0 ]]; then
    phase=address; write_journal intent ''; fence; runtime_guard
    value_is 0 address_count
    ip -4 address add "$host_prefix" dev "$iface"
    expected_address_digest=$(address_digest)
    added_address=1; write_journal intent ''
fi
if [[ "$prior_route_count" == 0 ]]; then
    phase=route; write_journal intent ''; fence; runtime_guard
    value_is 0 route_count
    ip -4 route add "$workload" via "$gateway" dev "$iface"
    expected_route_digest=$(route_digest)
    added_route=1; write_journal intent ''
fi
phase=readback
desired_runtime
fence
runtime_guard
verify_generated
observed_generated=$(artifact_digest '' nonselected)
[[ "$observed_generated" == "$generated_before" ]]
# Revalidate all source/ownership fences after the final live getters.
fence
phase=complete
write_journal complete ''
trap - ERR HUP INT TERM
jq 'del(.owner,.leaseBinding,.identity,.phase,.desired,.prior,.shadow) + {runtimeVerified:true,fileVerified:true,blocking:false}' "$journal"
