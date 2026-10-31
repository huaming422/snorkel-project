"""Centralized read-only filesystem tools + the agentic explore-and-emit loop.

Shared by the rubric and taste judges. litellm normalizes the *completion* API
across providers but ships no agent tools, so we declare our own: each tool is a
``Tool`` (its OpenAI-function schema + an ``impl``), registered by name.
``handle_tool_call`` dispatches through that registry — adding a tool is one
declaration, never a new branch.

Read-only by construction: the only side effects are reading files, listing
directories, and grep. Truncation caps bound how much any single call returns.
The judge explores with these tools and then submits a structured result via a
terminal "emit" tool — all in one litellm message thread, so the files it reads
stay in context through scoring.
"""

from __future__ import annotations

import json
import subprocess
import sys
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path
from typing import TYPE_CHECKING, Any

from pydantic import ValidationError

from . import llm_utils

if TYPE_CHECKING:
    from pydantic import BaseModel

MAX_FILE_LINES = 300
MAX_DIR_ENTRIES = 100
MAX_SEARCH_RESULTS = 50


@dataclass(frozen=True)
class Tool:
    """A read-only exploration tool: its schema plus the implementation.

    ``impl`` is called as ``impl(args, repo_path, max_file_lines)`` and returns
    the result text shown to the model.
    """

    name: str
    description: str
    parameters: dict[str, Any]
    impl: Callable[[dict[str, Any], Path, int], str]

    @property
    def spec(self) -> dict[str, Any]:
        """OpenAI-format tool entry for the litellm request."""
        return llm_utils.openai_tool(self.name, self.description, self.parameters)


# ── Tool implementations ────────────────────────────────────────────────────


def _int_arg(args: dict[str, Any], key: str, default: int) -> int:
    """Coerce a model-supplied numeric arg to int; models sometimes pass "10" for 10."""
    try:
        return int(args.get(key, default))
    except (TypeError, ValueError):
        return default


def _read_file(args: dict[str, Any], repo_path: Path, max_file_lines: int) -> str:
    fpath = repo_path / args["path"]
    if not fpath.exists():
        return f"[error: file not found: {args['path']}]"
    try:
        lines = fpath.read_text(errors="replace").splitlines()
    except OSError as e:
        return f"[error reading file: {e}]"
    start = max(0, _int_arg(args, "start_line", 1) - 1)
    selected = lines[start : _int_arg(args, "end_line", len(lines))]
    if len(selected) > max_file_lines:
        extra = len(selected) - max_file_lines
        selected = [*selected[:max_file_lines], f"... (truncated, {extra} more lines)"]
    return "\n".join(f"{start + i + 1}: {line}" for i, line in enumerate(selected))


def _list_directory(args: dict[str, Any], repo_path: Path, max_file_lines: int) -> str:
    del max_file_lines  # not applicable to directory listings
    dpath = repo_path / args["path"]
    if not dpath.exists():
        return f"[error: directory not found: {args['path']}]"
    try:
        entries = sorted(dpath.iterdir())
    except OSError as e:
        return f"[error listing directory: {e}]"
    result = [("d " if e.is_dir() else "f ") + e.name for e in entries[:MAX_DIR_ENTRIES]]
    if len(entries) > MAX_DIR_ENTRIES:
        result.append(f"... ({len(entries) - MAX_DIR_ENTRIES} more entries)")
    return "\n".join(result)


def _search_code(args: dict[str, Any], repo_path: Path, max_file_lines: int) -> str:
    del max_file_lines  # search is line-oriented, not file-windowed
    pattern = args["pattern"]
    file_glob = args.get("file_glob", "")
    cmd = (
        ["grep", "-rn", "--include", file_glob, pattern, str(repo_path)]
        if file_glob
        else ["grep", "-rn", pattern, str(repo_path)]
    )
    try:
        out = subprocess.run(cmd, capture_output=True, text=True, timeout=10, check=False)
    except (subprocess.SubprocessError, OSError) as e:
        return f"[error searching: {e}]"
    all_lines = out.stdout.splitlines()
    prefix = str(repo_path) + "/"
    cleaned = [ln.replace(prefix, "", 1) for ln in all_lines[:MAX_SEARCH_RESULTS]]
    if len(all_lines) > MAX_SEARCH_RESULTS:
        cleaned.append(f"... ({len(all_lines) - MAX_SEARCH_RESULTS} more matches)")
    return "\n".join(cleaned) if cleaned else "(no matches)"


READ_FILE = Tool(
    "read_file",
    "Read a file from the repository. Returns file content with line numbers. "
    "Use to examine source files for coding patterns, style, conventions.",
    {
        "type": "object",
        "properties": {
            "path": {"type": "string", "description": "File path relative to repo root."},
            "start_line": {"type": "integer", "description": "Start line (1-based). Optional."},
            "end_line": {"type": "integer", "description": "End line (inclusive). Optional."},
        },
        "required": ["path"],
    },
    _read_file,
)

LIST_DIRECTORY = Tool(
    "list_directory",
    "List files and directories. Use to discover project structure, find sibling modules, locate config files.",
    {
        "type": "object",
        "properties": {
            "path": {"type": "string", "description": "Directory path relative to repo root. '.' for root."},
        },
        "required": ["path"],
    },
    _list_directory,
)

SEARCH_CODE = Tool(
    "search_code",
    "Search for a regex pattern in the codebase. Returns matching lines with file paths. "
    "Use to find usage patterns and conventions.",
    {
        "type": "object",
        "properties": {
            "pattern": {"type": "string", "description": "Regex pattern to search for."},
            "file_glob": {"type": "string", "description": "File glob filter (e.g. '*.py'). Optional."},
        },
        "required": ["pattern"],
    },
    _search_code,
)

_REGISTRY: dict[str, Tool] = {t.name: t for t in (READ_FILE, LIST_DIRECTORY, SEARCH_CODE)}

# Tool sets (OpenAI specs): the rubric judge explores with read+list; taste adds search.
READ_TOOLS = [READ_FILE.spec, LIST_DIRECTORY.spec]
EXPLORE_TOOLS = [READ_FILE.spec, LIST_DIRECTORY.spec, SEARCH_CODE.spec]


def handle_tool_call(name: str, args: dict[str, Any], repo_path: Path, *, max_file_lines: int = MAX_FILE_LINES) -> str:
    """Dispatch a read-only tool call against ``repo_path`` via the registry.

    Any exception raised by a tool (e.g. a malformed model-supplied arg) is
    returned as an ``[error: ...]`` string so the model can self-correct on the
    next turn, rather than aborting the whole explore-and-emit thread.
    """
    tool = _REGISTRY.get(name)
    if tool is None:
        return f"[unknown tool: {name}]"
    try:
        return tool.impl(args, repo_path, max_file_lines)
    except Exception as e:  # noqa: BLE001
        return f"[error: {name} failed: {e}]"


# ── Explore-and-emit loop ─────────────────────────────────────────────────────


def _loads(arguments: str) -> dict[str, Any]:
    try:
        return json.loads(arguments)
    except (TypeError, json.JSONDecodeError):
        return {}


def _describe_call(call: Any) -> str:
    """One-line description of a tool call for the stderr trace (what was examined)."""
    name = getattr(call.function, "name", "?")
    args = _loads(getattr(call.function, "arguments", "") or "")
    if name == "read_file":
        sl, el = args.get("start_line"), args.get("end_line")
        rng = f"[{sl or ''}-{el or ''}]" if (sl or el) else ""
        return f"read_file: {args.get('path', '?')}{rng}"
    if name == "list_directory":
        return f"list_directory: {args.get('path', '?')}"
    if name == "search_code":
        return f"search_code: {args.get('pattern', '?')}"
    return name


def _assistant_message(msg: Any) -> dict[str, Any]:
    """Re-serialize a litellm assistant message (incl. tool_calls) as a plain dict."""
    out: dict[str, Any] = {"role": "assistant", "content": msg.content or ""}
    tcs = getattr(msg, "tool_calls", None)
    if tcs:
        out["tool_calls"] = [
            {
                "id": tc.id,
                "type": "function",
                "function": {"name": tc.function.name, "arguments": tc.function.arguments},
            }
            for tc in tcs
        ]
    return out


_EMIT_RETRIES = 2  # extra forced-emit attempts when the model's final args fail schema validation


def _validate_emit(schema: type[BaseModel], args: dict[str, Any] | None) -> tuple[Any, str | None]:
    """Validate emit args against the schema. Returns ``(model, None)`` on success,
    or ``(None, error)`` when args are missing/unparsed or fail validation — so the
    caller can feed the error back and let the model re-emit."""
    if args is None:
        return None, "no valid tool arguments were parsed"
    try:
        return schema.model_validate(args), None
    except ValidationError as e:
        return None, str(e)


def run_explore_and_emit(  # noqa: PLR0913
    *,
    model: str,
    system: str,
    user: str,
    repo_path: Path,
    explore_tools: list[dict[str, Any]],
    emit_tool_name: str,
    emit_schema: type[BaseModel],
    max_turns: int,
    max_tokens: int = 2048,
    max_file_lines: int = MAX_FILE_LINES,
    min_explore_turns: int = 0,
    label: str = "explore",
) -> Any | None:
    """Drive one explore→emit thread; return a validated ``emit_schema`` or ``None``.

    The model explores with ``explore_tools`` (read-only) and signals completion
    by calling ``emit_tool_name``, whose parameters are ``emit_schema``'s JSON
    schema. Scoring happens in-thread, so every file read stays in context.

    ``min_explore_turns`` enforces a floor: an emit attempted before that many
    turns is not accepted — the model is nudged to keep exploring (so it can't
    score blind). If the turn budget is exhausted without an emit, one final
    call forces it.
    """
    emit_tool = llm_utils.openai_tool(emit_tool_name, "Submit your final result.", emit_schema.model_json_schema())
    tools = [*explore_tools, emit_tool]
    messages: list[dict[str, Any]] = [{"role": "user", "content": user}]
    # Grading diagnostics: sizes only; tool output is unchanged.
    tool_chars = tool_bytes = 0

    for turn in range(max_turns):
        try:  # noqa: SIM105 - diagnostics must never abort the explore loop
            print(
                f"  [{label} turn {turn}] "
                f"request_chars={len(json.dumps(messages))} "
                f"tool_chars={tool_chars} tool_bytes={tool_bytes}",
                file=sys.stderr,
            )
        except Exception:  # noqa: S110
            pass
        response = llm_utils.complete(model=model, system=system, messages=messages, tools=tools, max_tokens=max_tokens)
        try:
            usage = getattr(response, "usage", None)
            print(f"  [{label} turn {turn}] input_tokens={getattr(usage, 'prompt_tokens', None)}", file=sys.stderr)
        except Exception:  # noqa: S110
            pass
        message = response.choices[0].message
        tool_calls = list(getattr(message, "tool_calls", None) or [])
        print(f"  [{label} turn {turn}] tools={len(tool_calls)}", file=sys.stderr)
        for call in tool_calls:
            print(f"      {_describe_call(call)}", file=sys.stderr)

        if not tool_calls:
            if turn < min_explore_turns:
                # Text-only response before the exploration floor. Falling through
                # to the forced emit here would let the model score blind, defeating
                # min_explore_turns — so nudge it to actually explore instead.
                messages.append(_assistant_message(message))
                messages.append(
                    {
                        "role": "user",
                        "content": (
                            f"Explore the codebase with the read-only tools first "
                            f"(at least {min_explore_turns} steps) before scoring, "
                            f"then call {emit_tool_name}."
                        ),
                    }
                )
                continue
            break  # text-only response at/after the floor; fall through to forced emit

        # Reply to every tool call so the thread stays valid: an emit before the
        # explore floor is nudged back to exploring; a valid emit returns; an emit
        # that fails schema validation is fed the error so the model re-emits;
        # real explore calls are executed (tool errors come back as [error: ...]).
        messages.append(_assistant_message(message))
        for call in tool_calls:
            if call.function.name == emit_tool_name:
                if turn < min_explore_turns:
                    content = (
                        f"Explore the codebase with the read-only tools before scoring "
                        f"(at least {min_explore_turns} steps), then call {emit_tool_name}."
                    )
                else:
                    parsed, err = _validate_emit(emit_schema, _loads(call.function.arguments))
                    if parsed is not None:
                        return parsed
                    content = (
                        f"Your {emit_tool_name} arguments were invalid ({err}). Call "
                        f"{emit_tool_name} again with every required field present and correctly typed."
                    )
            else:
                content = handle_tool_call(
                    call.function.name, _loads(call.function.arguments), repo_path, max_file_lines=max_file_lines
                )
                try:
                    tool_chars += len(content)
                    tool_bytes += len(content.encode("utf-8"))
                    print(
                        f"  [{label} turn {turn}] "
                        f"tool={call.function.name} "
                        f"chars={len(content)} "
                        f"bytes={len(content.encode('utf-8'))} "
                        f"total_chars={tool_chars} "
                        f"total_bytes={tool_bytes}",
                        file=sys.stderr,
                    )
                except Exception:  # noqa: S110
                    pass
            messages.append({"role": "tool", "tool_call_id": call.id, "content": content})

        if turn >= max_turns - 2:
            messages.append(
                {"role": "user", "content": f"You are low on exploration turns. Call {emit_tool_name} now."}
            )

    # Turns exhausted (or text-only stop): force the emit tool in-thread, with
    # bounded re-emits if the forced args fail schema validation.
    print(f"  [{label}] forcing emit in-context", file=sys.stderr)
    messages.append({"role": "user", "content": f"Call {emit_tool_name} now with your result."})
    for _ in range(_EMIT_RETRIES + 1):
        response = llm_utils.complete(
            model=model,
            system=system,
            messages=messages,
            tools=[emit_tool],
            tool_choice={"type": "function", "function": {"name": emit_tool_name}},
            max_tokens=max_tokens,
        )
        parsed, err = _validate_emit(emit_schema, llm_utils.tool_call_args(response, emit_tool_name))
        if parsed is not None:
            return parsed
        # Feed the validation error back as a proper tool reply so the thread stays
        # valid, then let the loop force another emit.
        print(f"  [{label}] forced emit invalid ({err}); retrying", file=sys.stderr)
        msg = response.choices[0].message
        messages.append(_assistant_message(msg))
        retry = f"Invalid ({err}). Re-call {emit_tool_name} with every required field present and correctly typed."
        emit_calls = list(getattr(msg, "tool_calls", None) or [])
        for call in emit_calls:
            messages.append({"role": "tool", "tool_call_id": call.id, "content": retry})
        if not emit_calls:
            messages.append({"role": "user", "content": retry})
    return None
