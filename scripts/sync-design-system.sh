#!/usr/bin/env bash
set -euo pipefail

# Vendor only dhub2-portal/docs/design-system at an immutable upstream commit.
# The private upstream stays out of the build graph; this repository consumes
# the reviewed snapshot under design-system/portal instead.

readonly UPSTREAM_REPOSITORY="dtonic/dhub2-portal"
readonly UPSTREAM_PATH="docs/design-system"

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
target_dir="$repo_root/design-system/portal"
upstream_ref="${1:-main}"

if [[ ! "$upstream_ref" =~ ^[A-Za-z0-9._/-]+$ ]]; then
  printf 'invalid upstream ref: %s\n' "$upstream_ref" >&2
  exit 2
fi

for command_name in gh base64; do
  if ! command -v "$command_name" >/dev/null 2>&1; then
    printf 'required command is missing: %s\n' "$command_name" >&2
    exit 2
  fi
done

upstream_commit="$(
  gh api "repos/$UPSTREAM_REPOSITORY/commits/$upstream_ref" --jq '.sha'
)"
if [[ ! "$upstream_commit" =~ ^[0-9a-f]{40}$ ]]; then
  printf 'failed to resolve immutable upstream commit for %s\n' "$upstream_ref" >&2
  exit 1
fi

entries="$(
  gh api "repos/$UPSTREAM_REPOSITORY/git/trees/$upstream_commit?recursive=1" \
    --jq ".tree[] | select(.type == \"blob\" and (.path | startswith(\"$UPSTREAM_PATH/\"))) | [.path, .sha] | @tsv" \
    | sort
)"
if [[ -z "$entries" ]]; then
  printf 'no files found under %s at %s\n' "$UPSTREAM_PATH" "$upstream_commit" >&2
  exit 1
fi

declare -A expected_files=()
while IFS=$'\t' read -r source_path blob_sha; do
  relative_path="${source_path#"$UPSTREAM_PATH/"}"
  if [[ -z "$relative_path" || "$relative_path" == ../* || "$relative_path" == */../* ]]; then
    printf 'unsafe upstream path: %s\n' "$source_path" >&2
    exit 1
  fi
  expected_files["$relative_path"]="$blob_sha"
done <<< "$entries"

# Upstream removals require an explicit review. Do not silently preserve stale
# files or delete local snapshot content during a sync.
if [[ -d "$target_dir" ]]; then
  while IFS= read -r -d '' existing_path; do
    relative_path="${existing_path#"$target_dir/"}"
    case "$relative_path" in
      UPSTREAM|MANIFEST) continue ;;
    esac
    if [[ -z "${expected_files[$relative_path]+present}" ]]; then
      printf 'stale snapshot file requires explicit removal: %s\n' "$relative_path" >&2
      exit 1
    fi
  done < <(find "$target_dir" -type f -print0)
fi

mkdir -p "$target_dir"
while IFS=$'\t' read -r source_path blob_sha; do
  relative_path="${source_path#"$UPSTREAM_PATH/"}"
  destination="$target_dir/$relative_path"
  encoded_content="$(
    gh api "repos/$UPSTREAM_REPOSITORY/git/blobs/$blob_sha" --jq '.content'
  )"
  mkdir -p "$(dirname -- "$destination")"
  printf '%s' "$encoded_content" | base64 --decode > "$destination"
done <<< "$entries"

{
  printf 'repository=%s\n' "$UPSTREAM_REPOSITORY"
  printf 'source=%s\n' "$UPSTREAM_PATH"
  printf 'ref=%s\n' "$upstream_ref"
  printf 'commit=%s\n' "$upstream_commit"
} > "$target_dir/UPSTREAM"

{
  printf '# Git blob SHA and path, sorted by upstream path.\n'
  while IFS=$'\t' read -r source_path blob_sha; do
    printf '%s  %s\n' "$blob_sha" "${source_path#"$UPSTREAM_PATH/"}"
  done <<< "$entries"
} > "$target_dir/MANIFEST"

printf 'synced %d files from %s@%s\n' \
  "${#expected_files[@]}" "$UPSTREAM_REPOSITORY" "$upstream_commit"
