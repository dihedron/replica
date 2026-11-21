# shellcheck shell=bash
# The WAN between the regions (.specs/veho-lab.md §3): the lab VM routes
# between the region bridges and delays, on each bridge's egress, the packets
# that come from another region. Partitions are nftables rules of the table
# inet veho_wan. Neither survives a reboot of the lab VM: lab_start applies
# what env.json recorded.

# The pairs of regions, in the order of the profiles' figures.
WAN_PAIRS=(a-b a-c b-c)

# wan_pair X Y: the pair of two regions, in WAN_PAIRS' spelling.
wan_pair() { if [[ "$1" < "$2" ]]; then echo "$1-$2"; else echo "$2-$1"; fi; }

# wan_netem SPEC: netem arguments for a link SPEC, DELAY[/JITTER[/LOSS]] in
# ms, ms and percent; empty when the link adds nothing.
wan_netem() {
	local delay jitter loss args=""
	IFS=/ read -r delay jitter loss <<<"$1"
	jitter="${jitter:-0}" loss="${loss:-0}"
	if [[ "$delay" != 0 || "$jitter" != 0 ]]; then
		args="delay ${delay}ms"
		[[ "$jitter" == 0 ]] || args+=" ${jitter}ms distribution normal"
	fi
	[[ "$loss" == 0 ]] || args+=" loss ${loss}%"
	[[ -z "$args" ]] || args+=" limit 100000"
	echo "$args"
}

# wan_check_spec SPEC: fails unless SPEC is DELAY[/JITTER[/LOSS]] of numbers.
wan_check_spec() {
	[[ "$1" =~ ^[0-9]+(\.[0-9]+)?(/[0-9]+(\.[0-9]+)?(/[0-9]+(\.[0-9]+)?)?)?$ ]] ||
		die "link \"$1\": DELAY[/JITTER[/LOSS]], in ms, ms and percent"
}

# wan_apply LINKS: applies LINKS, a JSON object from a pair (a-b, a-c, b-c)
# to its SPEC, and records it in env.json.
wan_apply() {
	local links="$1" script="" x y n band spec netem
	for x in "${REGIONS[@]}"; do
		script+="tc qdisc del dev region-$x root 2>/dev/null || true"$'\n'
		script+="tc qdisc add dev region-$x root handle 1: prio bands 3 priomap 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0"$'\n'
		band=2
		for y in "${REGIONS[@]}"; do
			[[ "$x" == "$y" ]] && continue
			spec="$(jq -r --arg p "$(wan_pair "$x" "$y")" '.[$p] // "0"' <<<"$links")"
			netem="$(wan_netem "$spec")"
			if [[ -n "$netem" ]]; then
				script+="tc qdisc add dev region-$x parent 1:$band handle ${band}0: netem $netem"$'\n'
			fi
			script+="tc filter add dev region-$x parent 1: protocol ip prio 1 u32 match ip src $(region_subnet "$y") flowid 1:$band"$'\n'
			band=$((band + 1))
		done
	done
	lxd exec lxd -- bash -euc "$script" || die "wan: cannot apply the queueing disciplines"
	env_set .wan.links "$links"
	n="$(jq -r 'to_entries | map("\(.key)=\(.value)") | join(" ")' <<<"$links")"
	info "WAN: $n (one-way ms[/jitter ms[/loss %]])"
}

# wan_profile_links PROFILE [JITTER] [LOSS]: the links of a profile of
# lab.env, as JSON, with JITTER (ms, or N% of each delay) and LOSS (percent).
wan_profile_links() {
	local var="LAB_WAN_PROFILE_$1" jitter="${2:-0}" loss="${3:-0}" delays i d j spec links='{}'
	[[ -n "${!var:-}" ]] || die "no WAN profile \"$1\": $(compgen -v LAB_WAN_PROFILE_ | sed 's/LAB_WAN_PROFILE_//' | xargs)"
	read -r -a delays <<<"${!var}"
	for i in 0 1 2; do
		d="${delays[$i]}"
		if [[ "$jitter" == *% ]]; then
			j="$(awk -v d="$d" -v p="${jitter%\%}" 'BEGIN {printf "%.2f", d * p / 100}')"
		else
			j="$jitter"
		fi
		spec="$d/$j/$loss"
		wan_check_spec "$spec"
		links="$(jq -c --arg p "${WAN_PAIRS[$i]}" --arg s "$spec" '.[$p] = $s' <<<"$links")"
	done
	echo "$links"
}

# wan_partition_apply PAIRS: replaces the partitions with PAIRS (a list of
# pairs, as JSON), and records them.
wan_partition_apply() {
	local pairs="$1" script p x y
	script="nft delete table inet veho_wan 2>/dev/null || true"$'\n'
	if [[ "$(jq length <<<"$pairs")" != 0 ]]; then
		script+="nft add table inet veho_wan"$'\n'
		script+="nft add chain inet veho_wan forward '{ type filter hook forward priority -10; policy accept; }'"$'\n'
		for p in $(jq -r '.[]' <<<"$pairs"); do
			x="${p%-*}" y="${p#*-}"
			script+="nft add rule inet veho_wan forward ip saddr $(region_subnet "$x") ip daddr $(region_subnet "$y") drop"$'\n'
			script+="nft add rule inet veho_wan forward ip saddr $(region_subnet "$y") ip daddr $(region_subnet "$x") drop"$'\n'
		done
	fi
	lxd exec lxd -- bash -euc "$script" || die "wan: cannot apply the partitions"
	env_set .wan.partitions "$pairs"
	if [[ "$(jq length <<<"$pairs")" == 0 ]]; then
		info "WAN: no partition"
	else
		info "WAN: partitioned $(jq -r 'join(", ")' <<<"$pairs")"
	fi
}

# wan_nat masquerades the regions' traffic that leaves the lab VM by its
# uplink (package downloads), and nothing else: the traffic between regions
# keeps its source address, which the WAN classifies (table ip veho_nat).
wan_nat() {
	lxd exec lxd -- bash -euc "
		uplink=\$(ip -4 route show default | awk '{print \$5; exit}')
		nft delete table ip veho_nat 2>/dev/null || true
		nft add table ip veho_nat
		nft add chain ip veho_nat postrouting '{ type nat hook postrouting priority srcnat; policy accept; }'
		nft add rule ip veho_nat postrouting oifname \"\$uplink\" ip saddr $LAB_REGION_PREFIX.0.0/16 masquerade
	" || die "wan: cannot set up the NAT of the regions"
}

# wan_restore applies what env.json recorded, or the default profile and no
# partition.
wan_restore() {
	local links pairs
	wan_nat
	links="$(env_get '.wan.links // empty' 2>/dev/null || true)"
	[[ -n "$links" ]] || links="$(wan_profile_links "$LAB_WAN_PROFILE")"
	pairs="$(env_get '.wan.partitions // empty' 2>/dev/null || true)"
	wan_apply "$links"
	wan_partition_apply "${pairs:-[]}"
}

# wan_default applies the default profile and heals every partition.
wan_default() {
	wan_apply "$(wan_profile_links "$LAB_WAN_PROFILE")"
	wan_partition_apply '[]'
}

cmd_wan() {
	require_lab
	local verb="${1:-show}"
	(($#)) && shift
	case "$verb" in
	show)
		section "WAN links (one-way ms[/jitter ms[/loss %]])"
		env_get '.wan.links // {} | to_entries[] | "        \(.key) \(.value)"'
		section "WAN partitions"
		env_get '.wan.partitions // [] | if length == 0 then "        none" else .[] | "        \(.)" end'
		;;
	set) wan_set "$@" ;;
	partition)
		local arg="${1:?wan partition: X (one region from the others) or X-Y}" pairs y
		pairs="$(env_get '.wan.partitions // []')"
		if [[ "$arg" == ?-? ]]; then
			region_check "${arg%-*}" && region_check "${arg#*-}"
			[[ "${arg%-*}" != "${arg#*-}" ]] || die "wan partition: two different regions"
			pairs="$(jq -c --arg p "$(wan_pair "${arg%-*}" "${arg#*-}")" '. + [$p] | unique' <<<"$pairs")"
		else
			region_check "$arg"
			for y in "${REGIONS[@]}"; do
				[[ "$y" == "$arg" ]] && continue
				pairs="$(jq -c --arg p "$(wan_pair "$arg" "$y")" '. + [$p] | unique' <<<"$pairs")"
			done
		fi
		wan_partition_apply "$pairs"
		;;
	heal) wan_partition_apply '[]' ;;
	*) die "wan: show, set, partition or heal" ;;
	esac
}

# wan_set PROFILE [--jitter J] [--loss L] | --link PAIR=SPEC... : sets the
# links from a profile, or changes single links of the current ones.
wan_set() {
	local profile="" jitter=0 loss=0 links edits=() e pair spec
	while (($#)); do
		case "$1" in
		--jitter) jitter="${2:?--jitter: ms or N%}" && shift 2 ;;
		--loss) loss="${2:?--loss: percent}" && shift 2 ;;
		--link) edits+=("${2:?--link: PAIR=DELAY[/JITTER[/LOSS]]}") && shift 2 ;;
		-*) die "wan set: unknown option $1" ;;
		*) profile="$1" && shift ;;
		esac
	done
	if [[ -n "$profile" ]]; then
		links="$(wan_profile_links "$profile" "$jitter" "$loss")"
	else
		((${#edits[@]})) || die "wan set: a profile, or --link PAIR=SPEC"
		links="$(env_get '.wan.links // {}')"
	fi
	for e in "${edits[@]}"; do
		pair="${e%%=*}" spec="${e#*=}"
		pair="$(tr '[:upper:]' '[:lower:]' <<<"$pair")"
		[[ " ${WAN_PAIRS[*]} " == *" $(wan_pair "${pair%-*}" "${pair#*-}") "* ]] || die "--link: no pair \"$pair\": a-b, a-c or b-c"
		wan_check_spec "$spec"
		links="$(jq -c --arg p "$(wan_pair "${pair%-*}" "${pair#*-}")" --arg s "$spec" '.[$p] = $s' <<<"$links")"
	done
	wan_apply "$links"
}
