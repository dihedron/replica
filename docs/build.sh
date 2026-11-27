#!/usr/bin/env bash
# Builds the release documents (see .specs/veho-docs.md) into
# build/docs/ (not dist/, which goreleaser cleans), with pinned container images: Mermaid diagrams to PDF with
# mermaid-cli, the charts of docs/common/data with gnuplot, then each document to
# PDF with pandoc, LaTeX and Eisvogel.
#
#   docs/build.sh                  # every document
#   docs/build.sh setup-guide      # one document
#
# DOCS_VERSION sets the version printed in the documents (default: git
# describe); DOCS_ENGINE the container engine (default: docker, else podman).
set -euo pipefail

readonly PANDOC_IMAGE="docker.io/pandoc/extra:3.11.0.0-debian@sha256:fa8ccae75418449568f10f1842706aee1885ab24fba54f5aec08516260fc0166"
readonly MERMAID_IMAGE="ghcr.io/mermaid-js/mermaid-cli/mermaid-cli:12.0.0@sha256:fa995339034aae7e5cd4f61482248b7f5c51be355b1a6f6eda11a2bbf8401f5f"
# bookworm's gnuplot, pinned by package version; the image at least two
# weeks old
readonly GNUPLOT_IMAGE="docker.io/library/debian:bookworm-20261027-slim@sha256:99c0b25c46d0214d5a2091092e7908907dcaecd6a962cde0ac700acee222b6f3"
readonly GNUPLOT_PACKAGE="gnuplot-nox=5.4.4+dfsg1-2+b2"
readonly DOCUMENTS=(setup-guide architecture operators-guide)

declare -A FILE_NAMES=(
	[setup-guide]=setup-guide
	[architecture]=architecture-and-design
	[operators-guide]=operators-guide
)

die() {
	echo "docs: $*" >&2
	exit 1
}

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo"

engine="${DOCS_ENGINE:-}"
if [[ -z "$engine" ]]; then
	if command -v docker >/dev/null; then engine=docker; elif command -v podman >/dev/null; then engine=podman; else die "neither docker nor podman is installed"; fi
fi
version="${DOCS_VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
version="${version#v}"
date="$(date -u +%Y-%m-%d)"

documents=("$@")
[[ ${#documents[@]} -gt 0 ]] || documents=("${DOCUMENTS[@]}")
for d in "${documents[@]}"; do
	[[ -n "${FILE_NAMES[$d]:-}" ]] || die "unknown document $d (one of: ${DOCUMENTS[*]})"
done

out=build/docs work=build/docs/work
rm -rf "$out"
mkdir -p "$work/diagrams"

# run IMAGE ARGS...: runs a container as the current user, with the
# repository mounted as its working directory
run() {
	local image="$1"
	shift
	# HOME: fontconfig and LaTeX need a writable cache for this user
	"$engine" run --rm --user "$(id -u):$(id -g)" --env HOME=/tmp --env LANG=C.UTF-8 --volume "$repo:/data" --workdir /data "$image" "$@"
}

# diagrams: each docs/common/diagrams/NAME.md holds one mermaid block
shopt -s nullglob
for source in docs/common/diagrams/*.md; do
	name="$(basename "$source" .md)"
	awk '/^```mermaid$/ { inside = 1; next } /^```$/ && inside { exit } inside' "$source" >"$work/diagrams/$name.mmd"
	[[ -s "$work/diagrams/$name.mmd" ]] || die "$source has no mermaid block"
	echo "docs: diagram $name"
	run "$MERMAID_IMAGE" --quiet --configFile /data/docs/common/mermaid.json \
		--input "/data/$work/diagrams/$name.mmd" --output "/data/$work/diagrams/$name.pdf" >/dev/null
done

# charts: the latency envelope's (docs/common/plots/latency-envelope.gp), from
# its data, redrawn by every build so that a release shows its regressions.
# gnuplot from Debian, installed in the pinned image (it has no image of its
# own); as root for apt, then the charts handed to the current user
data=docs/common/data/latency-envelope.csv
csvcol() { awk -F, -v n="$1" -v c="${2:-}" 'NR > 1 && (c == "" || $1 == c) && $8 == 0 && $n != "shift" && !seen[$n]++ { printf "%s ", $n }' "$data"; }
last="$(awk -F, 'END { print $1 }' "$data")"
byrtt="$work/latency-envelope-by-rtt.csv"
{ head -1 "$data" && tail -n +2 "$data" | sort -t, -k1,1 -k8,8n -k7,7 -k6,6n; } >"$byrtt"
echo "docs: charts of $data"
"$engine" run --rm --volume "$repo:/data" --workdir /data "$GNUPLOT_IMAGE" sh -c \
	"apt-get update -qq >/dev/null && { log=\$(DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends $GNUPLOT_PACKAGE 2>&1) || { echo \"\$log\" >&2; exit 1; }; } &&
	gnuplot -e \"data='$data'; byrtt='$byrtt'; out='$work/diagrams'; last='$last'; version='$(awk -F, 'END { print $2 }' "$data")';
		rtts='$(csvcol 6 "$last")'; jitters='$(csvcol 7 "$last")'; commits='$(csvcol 1)'; versions='$(csvcol 2)'\" docs/common/plots/latency-envelope.gp &&
	chown $(id -u):$(id -g) $work/diagrams/latency-envelope-*.pdf"

# the front matter, with the version and date
sed -e "s/\\\$version\\\$/$version/g" -e "s/\\\$date\\\$/$date/g" docs/common/front-matter.md >"$work/front-matter.md"

for d in "${documents[@]}"; do
	pdf="$out/veho-${FILE_NAMES[$d]}-$version.pdf"
	echo "docs: $pdf"
	# a document's own defaults, if any, override the shared ones
	own=()
	if [[ -f "docs/$d/defaults.yaml" ]]; then own=(--defaults "docs/$d/defaults.yaml"); fi
	# shellcheck disable=SC2046 # the chapters, in file order
	run "$PANDOC_IMAGE" \
		--defaults docs/common/defaults.yaml "${own[@]}" \
		--metadata-file "docs/$d/metadata.yaml" \
		--metadata date="Version $version · $date" \
		--variable header-right="Version $version" \
		--output "$pdf" \
		"$work/front-matter.md" $(ls docs/"$d"/[0-9][0-9]-*.md)
done
