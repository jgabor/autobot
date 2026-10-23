#!/usr/bin/env bash

set -uo pipefail

SHUTDOWN_REQUESTED=0
trap 'SHUTDOWN_REQUESTED=1' TERM

die() {
    echo "autobot: $*" >&2
    exit 1
}

usage() {
    cat <<'EOF'
Usage: autobot.sh [--help]

Runs the numbered Markdown tasks from the autobot configuration directory once.

Options:
  -h, --help   Show this help

Environment variables:
  AUTOBOT_PROVIDER              OpenAI-compatible endpoint URL (required)
  AUTOBOT_MODEL                 model name (required)
  OPENAI_API_KEY                API key for OpenAI-compatible endpoints, if required
  OPENCODE_API_KEY              API key for OpenCode endpoints, if required
  AUTOBOT_TIMEOUT               per-request timeout in seconds (default: 300)
  AUTOBOT_MAX_TOOL_ROUNDS       maximum tool-call rounds per task (default: 20)
  AUTOBOT_MAX_TOOL_OUTPUT       maximum characters kept per tool output (default: 12000)
  AUTOBOT_MAX_TOOL_RESULTS      maximum results per Grep or Glob call (default: 500)

Exit codes:
  0   all tasks completed successfully
  1   configuration error, runtime error, or task failure
EOF
}

[[ -n "${HOME:-}" ]] || die "HOME is not set"

CONFIG_HOME=${XDG_CONFIG_HOME:-$HOME/.config}
STATE_HOME=${XDG_STATE_HOME:-$HOME/.local/state}
CONFIG_DIR=$CONFIG_HOME/autobot
STATE_DIR=$STATE_HOME/autobot
SYSTEM_FILE=$CONFIG_DIR/0-system.md
STATE_FILE=$STATE_DIR/state.md

AUTOBOT_TIMEOUT=${AUTOBOT_TIMEOUT:-300}
AUTOBOT_MAX_TOOL_ROUNDS=${AUTOBOT_MAX_TOOL_ROUNDS:-20}
AUTOBOT_MAX_TOOL_OUTPUT=${AUTOBOT_MAX_TOOL_OUTPUT:-12000}
AUTOBOT_MAX_TOOL_RESULTS=${AUTOBOT_MAX_TOOL_RESULTS:-500}
MAX_RUN_REPORT=40000
MAX_MESSAGES=100000

require_command() {
    command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

check_config() {
    [[ "$CONFIG_HOME" == /* ]] || die "XDG_CONFIG_HOME must be an absolute path"
    [[ "$STATE_HOME" == /* ]] || die "XDG_STATE_HOME must be an absolute path"
    [[ -n "${AUTOBOT_PROVIDER:-}" ]] || die "AUTOBOT_PROVIDER is not set"
    [[ -n "${AUTOBOT_MODEL:-}" ]] || die "AUTOBOT_MODEL is not set"
    [[ "$AUTOBOT_TIMEOUT" =~ ^[1-9][0-9]*$ ]] ||
        die "AUTOBOT_TIMEOUT must be a positive number"
    [[ "$AUTOBOT_MAX_TOOL_ROUNDS" =~ ^[1-9][0-9]*$ ]] ||
        die "AUTOBOT_MAX_TOOL_ROUNDS must be a positive number"
    [[ "$AUTOBOT_MAX_TOOL_OUTPUT" =~ ^[1-9][0-9]*$ ]] ||
        die "AUTOBOT_MAX_TOOL_OUTPUT must be a positive number"
    [[ "$AUTOBOT_MAX_TOOL_RESULTS" =~ ^[1-9][0-9]*$ ]] ||
        die "AUTOBOT_MAX_TOOL_RESULTS must be a positive number"

    case "$AUTOBOT_PROVIDER" in
    http://* | https://*) ;;
    *) die "AUTOBOT_PROVIDER must be an HTTP(S) provider endpoint" ;;
    esac
}

provider_endpoint() {
    local base=${AUTOBOT_PROVIDER%/}

    case "$base" in
    */chat/completions) echo "$base" ;;
    */v1) echo "$base/chat/completions" ;;
    *) echo "$base/v1/chat/completions" ;;
    esac
}

provider_api_key() {
    case "$AUTOBOT_PROVIDER" in
    *opencode*) printf '%s' "${OPENCODE_API_KEY:-}" ;;
    *) printf '%s' "${OPENAI_API_KEY:-}" ;;
    esac
}

prepare_files() {
    [[ -d "$CONFIG_DIR" && -r "$CONFIG_DIR" && -x "$CONFIG_DIR" ]] ||
        die "configuration directory is not accessible: $CONFIG_DIR"
    [[ -r "$SYSTEM_FILE" ]] || die "system file not found or unreadable: $SYSTEM_FILE"

    mkdir -p "$STATE_DIR" || die "could not create state directory: $STATE_DIR"
    [[ -d "$STATE_DIR" && -w "$STATE_DIR" ]] ||
        die "state directory is not writable: $STATE_DIR"
    if [[ ! -e "$STATE_FILE" ]]; then
        printf '%s\n' \
            '# autobot state' \
            '' \
            'Version: 1' \
            'Last updated: never' \
            'Run count: 0' \
            '' \
            '## Important locations' \
            '' \
            '## Useful procedures' \
            '' \
            '## Recurring gotchas' \
            '' \
            '## Durable insights' \
            '' \
            '## Open follow-ups' \
            '' \
            '## Next-run improvements' \
            '' \
            '- No improvements recorded yet.' \
            >"$STATE_FILE" || die "could not create state file: $STATE_FILE"
    fi
    [[ -f "$STATE_FILE" && -r "$STATE_FILE" && -w "$STATE_FILE" ]] ||
        die "state file is not a readable, writable regular file: $STATE_FILE"
}

acquire_lock() {
    exec 9>"$STATE_DIR/autobot.lock" ||
        die "could not open lock file: $STATE_DIR/autobot.lock"
    flock -n 9 || die "another autobot instance is already running"
}

collect_tasks() {
    local path name number
    local -a candidates=()
    declare -A seen_numbers=()

    shopt -s nullglob
    for path in "$CONFIG_DIR"/*.md; do
        name=${path##*/}
        [[ "$name" =~ ^([1-9][0-9]*)-.+\.md$ ]] || continue
        candidates+=("$path")
    done
    shopt -u nullglob

    ((${#candidates[@]} > 0)) || die "no numbered task files found in $CONFIG_DIR"

    mapfile -t TASKS < <(printf '%s\n' "${candidates[@]}" | LC_ALL=C sort -V)

    for path in "${TASKS[@]}"; do
        name=${path##*/}
        number=${name%%-*}
        if [[ -n "${seen_numbers[$number]+set}" ]]; then
            die "multiple tasks use the number $number"
        fi
        seen_numbers[$number]=1
    done
}

resolve_path() {
    local path=$1
    case "$path" in
    \~ | \~/*) path="$HOME${path:1}" ;;
    esac
    printf '%s\n' "$path"
}

self_check() {
    local task_file

    echo "autobot: running self-check"
    require_command curl
    require_command flock
    require_command head
    require_command jq
    require_command date
    require_command rg
    require_command sed
    require_command sort
    require_command mktemp
    check_config
    prepare_files
    collect_tasks

    for task_file in "${TASKS[@]}"; do
        [[ -f "$task_file" && -r "$task_file" ]] ||
            die "task file is not a readable regular file: $task_file"
    done

    [[ -d "$PWD" && -r "$PWD" && -x "$PWD" ]] ||
        die "current working directory is not accessible: $PWD"

    TOOLS_JSON=$(tool_definitions) || die "could not build tool definitions"
    echo "autobot: self-check passed"
}

tool_read() {
    local parameters=$1
    local path offset limit

    path=$(jq -r '.path // empty' <<<"$parameters")
    offset=$(jq -r '.offset // 0' <<<"$parameters")
    limit=$(jq -r '.limit // 200' <<<"$parameters")
    [[ -n "$path" ]] || { echo "Read requires a path"; return 1; }
    [[ "$offset" =~ ^[0-9]+$ ]] || { echo "Read offset must be a non-negative number"; return 1; }
    [[ "$limit" =~ ^[1-9][0-9]*$ ]] || { echo "Read limit must be a positive number"; return 1; }

    path=$(resolve_path "$path")
    [[ -r "$path" ]] || { echo "File is not readable: $path"; return 1; }
    sed -n "$((offset + 1)),$((offset + limit))p" "$path"
}

tool_write() {
    local parameters=$1
    local path content mode

    path=$(jq -r '.path // empty' <<<"$parameters")
    content=$(jq -r '.content // empty' <<<"$parameters")
    mode=$(jq -r '.mode // "w"' <<<"$parameters")
    [[ -n "$path" ]] || { echo "Write requires a path"; return 1; }
    [[ "$mode" == w || "$mode" == a ]] || { echo "Write mode must be 'w' or 'a'"; return 1; }

    path=$(resolve_path "$path")
    [[ "$path" != "$STATE_FILE" && "$path" != "$SYSTEM_FILE" ]] || {
        echo "This file is managed by autobot and cannot be edited directly: $path"
        return 1
    }
    if [[ -e "$path" ]]; then
        [[ -w "$path" ]] || { echo "File is not writable: $path"; return 1; }
    else
        [[ -d "$(dirname "$path")" && -w "$(dirname "$path")" ]] || {
            echo "File or writable parent directory not found: $path"
            return 1
        }
    fi

    if [[ "$mode" == a ]]; then
        printf '%s' "$content" >>"$path"
        printf 'Appended to %s\n' "$path"
    else
        printf '%s' "$content" >"$path"
        printf 'Wrote %s\n' "$path"
    fi
}

tool_edit() {
    local parameters=$1
    local path old_string new_string replace_all source updated

    path=$(jq -r '.path // empty' <<<"$parameters")
    old_string=$(jq -r '.oldString // empty' <<<"$parameters")
    new_string=$(jq -r '.newString // empty' <<<"$parameters")
    replace_all=$(jq -r '.replaceAll // false' <<<"$parameters")
    [[ -n "$path" ]] || { echo "Edit requires a path"; return 1; }
    [[ -n "$old_string" ]] || { echo "Edit requires a non-empty oldString"; return 1; }
    [[ "$replace_all" == true || "$replace_all" == false ]] || {
        echo "Edit replaceAll must be a boolean"
        return 1
    }

    path=$(resolve_path "$path")
    [[ "$path" != "$STATE_FILE" && "$path" != "$SYSTEM_FILE" ]] || {
        echo "This file is managed by autobot and cannot be edited directly: $path"
        return 1
    }
    [[ -f "$path" && -r "$path" && -w "$path" ]] || {
        echo "File is not a readable, writable regular file: $path"
        return 1
    }

    source=$(<"$path")
    [[ "$source" == *"$old_string"* ]] || {
        echo "oldString was not found in $path"
        return 1
    }

    if [[ "$replace_all" == true ]]; then
        updated=${source//"$old_string"/"$new_string"}
    else
        updated=${source/"$old_string"/"$new_string"}
    fi
    printf '%s' "$updated" >"$path" || return 1
    printf 'Edited %s\n' "$path"
}

tool_grep() {
    local parameters=$1
    local pattern path include output status errors
    local -a args=(-nH --no-ignore --field-match-separator='|' --regexp)

    pattern=$(jq -r '.pattern // empty' <<<"$parameters")
    path=$(jq -r '.path // "."' <<<"$parameters")
    include=$(jq -r '.include // empty' <<<"$parameters")
    [[ -n "$pattern" ]] || { echo "Grep requires a pattern"; return 1; }
    path=$(resolve_path "$path")
    [[ -e "$path" ]] || { echo "Path not found: $path"; return 1; }
    [[ -n "$include" ]] && args+=(--glob "$include")
    args+=("$pattern" "$path")

    local stderr_file
    stderr_file=$(mktemp) || return 1
    output=$(rg "${args[@]}" 2>"$stderr_file" | head -n "$MAX_TOOL_RESULTS")
    status=$?
    errors=$(<"$stderr_file")
    rm -f "$stderr_file"

    case "$status" in
    0) printf '%s\n' "$output" ;;
    141) printf '%s\n[results limited to %s lines]\n' "$output" "$AUTOBOT_MAX_TOOL_RESULTS" ;;
    1) printf 'No matches found.\n' ;;
    *) printf '%s\n' "$errors"; return "$status" ;;
    esac
}

tool_glob() {
    local parameters=$1
    local pattern root glob match found=0
    local -a matches=()

    pattern=$(jq -r '.pattern // empty' <<<"$parameters")
    root=$(jq -r '.root_dir // "."' <<<"$parameters")
    [[ -n "$pattern" ]] || { echo "Glob requires a pattern"; return 1; }
    root=$(resolve_path "$root")
    [[ -d "$root" ]] || { echo "Directory not found: $root"; return 1; }

    case "$pattern" in
    \~ | \~/*) pattern=$(resolve_path "$pattern") ;;
    esac
    if [[ "$pattern" == /* ]]; then
        glob=$pattern
    else
        glob=$root/$pattern
    fi

    shopt -s globstar nullglob
    mapfile -t matches < <(compgen -G "$glob" | head -n "$AUTOBOT_MAX_TOOL_RESULTS")
    shopt -u globstar nullglob

    for match in "${matches[@]}"; do
        [[ -f "$match" ]] || continue
        printf '%s\n' "$match"
        found=1
    done
    ((found)) || printf 'No matches found for %s\n' "$glob"
}

tool_definitions() {
    jq -n '[
      {
        type: "function",
        function: {
          name: "Read",
          description: "Read lines from a local file.",
          parameters: {
            type: "object",
            properties: {
              path: {type: "string"},
              offset: {type: "integer", minimum: 0, default: 0},
              limit: {type: "integer", minimum: 1, default: 200}
            },
            required: ["path"]
          }
        }
      },
      {
        type: "function",
        function: {
          name: "Write",
          description: "Write or append text to a local file.",
          parameters: {
            type: "object",
            properties: {
              path: {type: "string"},
              content: {type: "string"},
              mode: {type: "string", enum: ["w", "a"], default: "w"}
            },
            required: ["path", "content"]
          }
        }
      },
      {
        type: "function",
        function: {
          name: "Edit",
          description: "Replace an exact string in a local file.",
          parameters: {
            type: "object",
            properties: {
              path: {type: "string"},
              oldString: {type: "string"},
              newString: {type: "string"},
              replaceAll: {type: "boolean", default: false}
            },
            required: ["path", "oldString", "newString"]
          }
        }
      },
      {
        type: "function",
        function: {
          name: "Grep",
          description: "Search local file contents with a regular expression.",
          parameters: {
            type: "object",
            properties: {
              pattern: {type: "string"},
              path: {type: "string", default: "."},
              include: {type: "string"}
            },
            required: ["pattern"]
          }
        }
      },
      {
        type: "function",
        function: {
          name: "Glob",
          description: "Find local files matching a glob pattern. Supports *, **, and ?. Does not support brace expansion (e.g. {js,ts}); issue separate calls for each pattern. Prefer narrow patterns and avoid scanning an entire home directory.",
          parameters: {
            type: "object",
            properties: {
              pattern: {type: "string"},
              root_dir: {type: "string", default: "."}
            },
            required: ["pattern"]
          }
        }
      }
    ]'
}

execute_tool() {
    local name=$1
    local parameters=$2

    case "$name" in
    Read) tool_read "$parameters" ;;
    Write) tool_write "$parameters" ;;
    Edit) tool_edit "$parameters" ;;
    Grep) tool_grep "$parameters" ;;
    Glob) tool_glob "$parameters" ;;
    *) echo "Unknown tool: $name"; return 1 ;;
    esac
}

truncate_tool_output() {
    local output=$1

    if ((${#output} > AUTOBOT_MAX_TOOL_OUTPUT)); then
        printf '%s\n[tool output truncated]\n' "${output:0:AUTOBOT_MAX_TOOL_OUTPUT}"
    else
        printf '%s' "$output"
    fi
}

complete_task() {
    local task_file=$1
    local system_prompt state task messages response assistant content
    local tool_count round=0
    local tool_call tool_id tool_name arguments tool_output
    local tools="$TOOLS_JSON"

    system_prompt=$(<"$SYSTEM_FILE")
    state=$(<"$STATE_FILE")
    task=$(<"$task_file")
    messages=$(jq -n \
        --arg system "$system_prompt" \
        --arg state "$state" \
        --arg task "$task" \
        '[
          {role: "system", content: ($system + "\n\nPrevious state:\n" + $state)},
          {role: "user", content: $task}
        ]') || die "could not build task messages"

    while ((round < AUTOBOT_MAX_TOOL_ROUNDS)); do
        if ! response=$(provider_request "$messages" "$tools"); then
            echo "provider request failed for ${task_file##*/}:" >&2
            return 1
        fi

        if ! assistant=$(jq -c '.choices[0].message // empty' <<<"$response"); then
            echo "provider returned invalid JSON for ${task_file##*/}:" >&2
            echo "$response" >&2
            return 1
        fi
        [[ -n "$assistant" && "$assistant" != null ]] || {
            echo "provider returned no assistant message for ${task_file##*/}:" >&2
            echo "$response" >&2
            return 1
        }

        tool_count=$(jq -r '.tool_calls // [] | length' <<<"$assistant") || return 1
        if ((tool_count == 0)); then
            content=$(jq -r '.content // empty' <<<"$assistant")
            [[ -n "$content" ]] || {
                echo "provider returned no text for ${task_file##*/}:" >&2
                echo "$response" >&2
                return 1
            }
            printf '%s\n' "$content"
            return 0
        fi

        messages=$(jq --argjson assistant "$assistant" '. + [$assistant]' <<<"$messages") || return 1
        while IFS= read -r tool_call; do
            tool_id=$(jq -r '.id // empty' <<<"$tool_call")
            tool_name=$(jq -r '.function.name // empty' <<<"$tool_call")
            arguments=$(jq -r '.function.arguments // "{}"' <<<"$tool_call")
            [[ -n "$tool_name" ]] || {
                tool_output="Invalid tool call: missing function name"
            }

            echo "  tool: $tool_name" >&2
            if [[ -n "$tool_name" ]]; then
                if tool_output=$(execute_tool "$tool_name" "$arguments"); then
                    tool_output=$(truncate_tool_output "$tool_output")
                else
                    tool_output="Tool error: $tool_output"
                fi
            fi

            messages=$(jq \
                --arg id "$tool_id" \
                --arg name "$tool_name" \
                --arg content "$tool_output" \
                '. + [{role: "tool", tool_call_id: $id, name: $name, content: $content}]' \
                <<<"$messages") || return 1
        done < <(jq -c '.tool_calls[]' <<<"$assistant")

        ((round++))
    done

    echo "maximum tool-call rounds reached for ${task_file##*/}" >&2
    return 1
}

provider_request() {
    local messages=$1
    local tools=${2:-[]}
    local payload response endpoint api_key

    if ((${#messages} > MAX_MESSAGES)); then
        echo "conversation context exceeded ${MAX_MESSAGES} characters" >&2
        return 1
    fi

    endpoint=$(provider_endpoint)
    api_key=$(provider_api_key)
    payload=$(jq -n \
        --arg model "$AUTOBOT_MODEL" \
        --argjson messages "$messages" \
        --argjson tools "$tools" \
        '{
          model: $model,
          messages: $messages,
          stream: false
        } | if ($tools | length) > 0 then .tools = $tools | .tool_choice = "auto" else . end') || {
        echo "could not build provider request" >&2
        return 1
    }

    local -a curl_args=(
        --fail-with-body
        --silent
        --show-error
        --max-time "$AUTOBOT_TIMEOUT"
        --header 'Content-Type: application/json'
    )
    [[ -n "$api_key" ]] && curl_args+=(--header "Authorization: Bearer $api_key")

    if ! response=$(curl "${curl_args[@]}" --data "$payload" "$endpoint" 2>&1); then
        echo "$response" >&2
        return 1
    fi
    printf '%s' "$response"
}

append_run_report() {
    local task_file=$1
    local status=$2
    local result=$3

    RUN_REPORT+=$'\n### '
    RUN_REPORT+="${task_file##*/}"
    RUN_REPORT+=$'\nStatus: '
    RUN_REPORT+="$status"
    RUN_REPORT+=$'\n\nResult:\n'
    RUN_REPORT+="$result"
    RUN_REPORT+=$'\n'

    if ((${#RUN_REPORT} > MAX_RUN_REPORT)); then
        RUN_REPORT=${RUN_REPORT: -MAX_RUN_REPORT}
    fi
}

run_reflection() {
    local system_prompt state messages response content current_count next_count now
    local reflection_instructions temporary_file heading

    system_prompt=$(<"$SYSTEM_FILE")
    state=$(<"$STATE_FILE")
    current_count=$(sed -nE 's/^Run count: *([0-9]+).*/\1/p' "$STATE_FILE")
    current_count=${current_count%%$'\n'*}
    [[ "$current_count" =~ ^[0-9]+$ ]] || current_count=0
    next_count=$((current_count + 1))
    now=$(date -Iseconds)

    read -r -d '' reflection_instructions <<EOF || :
You are performing autobot's post-run reflection.

Conduct a deep and honest introspection of this run. Reflect on:

- Assumptions that did not hold
- Actions that failed, and why
- Recurring mistakes or gotchas
- Useful files, locations, commands, and procedures discovered
- Insights that would genuinely help future runs
- Unresolved problems or follow-up work

Separate one-off events from durable knowledge. Only preserve information that is likely to be useful in a future run. Remove duplicates and stale information from the existing state.

Identify exactly three concrete things autobot should avoid or improve before the next run.

Do not include secrets, raw transcripts, verbose narration, or unsupported assumptions. Do not copy this prompt or the run report. Do not modify tasks or the system prompt.

Return only a complete replacement for state.md, beginning with '# autobot state', using these sections:

# autobot state

Version: 1
Last updated: $now
Run count: $next_count

## Important locations

## Useful procedures

## Recurring gotchas

## Durable insights

## Open follow-ups

## Next-run improvements

Include exactly three bullet points in ## Next-run improvements.
EOF

    messages=$(jq -n \
        --arg system "$system_prompt" \
        --arg instructions "$reflection_instructions" \
        --arg state "$state" \
        --arg report "$RUN_REPORT" \
        '[
          {role: "system", content: ($system + "\n\n" + $instructions)},
          {role: "user", content: ("Existing state:\n" + $state + "\n\nRun report:\n" + $report)}
        ]') || return 1

    echo "autobot: reflecting on the run" >&2
    if ! response=$(provider_request "$messages" '[]'); then
        echo "autobot: reflection failed; keeping the previous state" >&2
        return 1
    fi
    content=$(jq -r '.choices[0].message.content // empty' <<<"$response") || return 1
    content=$(sed -e '1{/^```/d;}' -e '${/^```$/d;}' <<<"$content")

    [[ "$content" == "# autobot state"* ]] || {
        echo "autobot: reflection did not return a valid state file" >&2
        return 1
    }
    for heading in \
        '## Important locations' \
        '## Useful procedures' \
        '## Recurring gotchas' \
        '## Durable insights' \
        '## Open follow-ups' \
        '## Next-run improvements'; do
        [[ "$content" == *"$heading"* ]] || {
            echo "autobot: reflection state is missing $heading" >&2
            return 1
        }
    done

    local improvements improvement_count=0
    improvements=${content#*'## Next-run improvements'}
    improvements=${improvements%%$'\n## '*}
    while IFS= read -r heading; do
        [[ "$heading" =~ ^[[:space:]]*[-*][[:space:]]+ ]] && ((improvement_count++))
    done <<<"$improvements"
    [[ "$improvement_count" -eq 3 ]] || {
        echo "autobot: reflection must contain exactly three next-run improvements (found $improvement_count)" >&2
        return 1
    }

    temporary_file=$(mktemp "$STATE_DIR/.state.XXXXXX") || return 1
    if ! printf '%s\n' "$content" >"$temporary_file"; then
        rm -f "$temporary_file"
        return 1
    fi
    if ! mv -f "$temporary_file" "$STATE_FILE"; then
        rm -f "$temporary_file"
        return 1
    fi
    echo "autobot: state updated" >&2
}

main() {
    case "${1:-}" in
    "") ;;
    --help | -h)
        usage
        return 0
        ;;
    *) die "unknown argument: $1" ;;
    esac

    self_check
    acquire_lock

    local task_output failed=0
    RUN_REPORT="# Run report"

    echo "autobot: running ${#TASKS[@]} task(s)"
    for task_file in "${TASKS[@]}"; do
        ((SHUTDOWN_REQUESTED)) && {
            echo "autobot: shutdown requested; stopping before ${task_file##*/}" >&2
            break
        }
        echo
        echo "==> ${task_file##*/}"
        if task_output=$(complete_task "$task_file"); then
            printf '%s\n' "$task_output"
            append_run_report "$task_file" "completed" "$task_output"
        else
            append_run_report "$task_file" "failed" "No final response was produced."
            failed=1
            break
        fi
    done

    run_reflection || true
    return "$failed"
}

main "$@"
