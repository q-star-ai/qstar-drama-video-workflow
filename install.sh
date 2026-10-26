#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
skill_source="$repo_root/skills/qstar-drama-video"

for skill_parent in "$HOME/.codex/skills" "$HOME/.claude/skills" "$HOME/.config/opencode/skills" "$HOME/.agents/skills" "$HOME/.openclaw/skills"; do
  mkdir -p "$skill_parent"
  destination="$skill_parent/qstar-drama-video"
  if [ -L "$destination" ]; then
    current=$(readlink "$destination")
    if [ "$current" = "$skill_source" ]; then
      printf 'already installed: %s\n' "$destination"
      continue
    fi
    printf 'refusing to replace existing link: %s\n' "$destination" >&2
    exit 1
  fi
  if [ -e "$destination" ]; then
    printf 'refusing to replace existing skill: %s\n' "$destination" >&2
    exit 1
  fi
  ln -s "$skill_source" "$destination"
  printf 'installed: %s\n' "$destination"
done
