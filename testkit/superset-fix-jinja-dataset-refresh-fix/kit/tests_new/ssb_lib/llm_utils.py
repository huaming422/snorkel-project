"""Shared LLM client/creds/model/retry helpers for the reward judges.

All judge model calls go through this module's :func:`complete` /
:func:`complete_structured`, which wrap ``litellm.completion`` — so the judges
are model-agnostic (any popular frontier model litellm supports) and do not
depend on the Anthropic SDK. Routing is two rules: an Anthropic model carries
cache_control markers, and a Portkey key routes through Portkey. Model slugs are
passed through as the caller wrote them.

``litellm`` is imported lazily inside the call helpers, so ``import llm_utils``
never fails on a missing SDK. Reached under both the ``src.python.ssb_lib``
(offline) and ``ssb_lib`` (in-container) namespaces, so intra-package imports
stay relative.
"""

from __future__ import annotations

import dataclasses
import json
import os
import time
from typing import TYPE_CHECKING, Any

if TYPE_CHECKING:
    from pydantic import BaseModel

# Default judge/classifier models, overridden by the SSB_OVERRIDE_* env vars.
JUDGE_MODEL_DEFAULT = "anthropic/claude-sonnet-4-6"
CLASSIFIER_MODEL_DEFAULT = "anthropic/claude-haiku-4-5"

_RETRY_ATTEMPTS = 4

# gpt-5.x/o-series reject tools+reasoning on chat/completions; setting reasoning_effort
# makes litellm bridge them to the Responses API, which returns chat-shaped output.
_OPENAI_REASONING_EFFORT = "medium"

# Any model can return no tool call despite a forced tool_choice; re-issue up to this many times.
_FORCED_TOOL_RETRIES = 3

# Substrings marking a transient (retryable) error, matched case-insensitively
# against ``str(exc)``. Kept specific ("rate limit", not bare "rate") and free of
# bare numbers — a digit run like "500" matches far too much error text (paths,
# token counts, line numbers). HTTP status codes are classified separately,
# against the exception's structured ``status_code``, never by substring.
_TRANSIENT_MARKERS = (
    "rate limit",
    "rate_limit",
    "ratelimit",
    "too many requests",
    "overloaded",
    "timeout",
    "timed out",
    "connection",
    "temporarily",
)

# Transient/retryable HTTP status codes, matched against the exception's
# ``status_code`` attribute (litellm exposes it) — not its message text.
_TRANSIENT_STATUS_CODES = frozenset({429, 500, 502, 503, 529})


_PORTKEY_BASE_URL = "https://api.portkey.ai"

# Gateway routing key. We standardize on PORTKEY_API_KEY for everything — judges
# and validation agent alike — so there is a single key to set and forward. The
# old per-role names (VAL_AGENT_PORTKEY_KEY / CC_VAL_PORTKEY_KEY) are no longer
# consulted.
_ROUTER_KEY_VARS = ("PORTKEY_API_KEY",)

# Per-role model overrides (settable via verifier env / harbor ``--ve``). Each
# replaces its role's default model with the given slug — a Portkey
# ``@provider/model`` catalog slug when a gateway key is set, else a litellm
# provider slug like ``openai/gpt-5.5``. The judge override covers the rubric,
# taste, and validation-review judges (one model for all three); the classifier
# override is separate so the cheap structural classifier can stay independent.
JUDGE_MODEL_OVERRIDE_VAR = "SSB_OVERRIDE_ALL_JUDGE_MODEL"
CLASSIFIER_MODEL_OVERRIDE_VAR = "SSB_OVERRIDE_CLASSIFIER_MODEL"
# Optional comma-separated model panel for the rubric + taste judges only (not the
# validation-review judge). Layered above the single-model override; see judge_panel.
PANEL_MODEL_OVERRIDE_VAR = "SSB_OVERRIDE_TASTE_RUBRIC_PANEL"


def resolve_portkey_key() -> str | None:
    """Return the gateway routing key from the first env var that is set."""
    for var in _ROUTER_KEY_VARS:
        if os.environ.get(var):
            return os.environ[var]
    return None


def fix_or_feature(task_type: str | None) -> str:
    """Collapse a 4-valued taxonomy ``task_type`` (bug/feature/performance/
    migration) to the binary fix/feature segment used across the bench (bundle
    suffixes, picker mix, Portkey ``task_type`` tag). Unknown/empty -> "" so
    the metadata assembler drops the key rather than tagging a guess."""
    if not task_type:
        return ""
    t = str(task_type).lower()
    if t in ("bug", "performance", "fix"):
        return "fix"
    if t in ("feature", "migration"):
        return "feature"
    return ""


def portkey_metadata_json(component: str, *, phase: str | None = None, extra: dict[str, str] | None = None) -> str:
    """Flat JSON object for the ``x-portkey-metadata`` header.

    The single place metadata is assembled, so a new call site can't ship
    untagged. Layered lowest-to-highest precedence:

    1. defaults (engagement_name, phase — ``phase`` arg, else the
       ``STAFF_BENCH_PHASE`` env set by non-task-gen pipelines such as
       final-grading, else ``task-gen``),
    2. any inherited ``SSB_PORTKEY_METADATA`` env JSON — the launch layer's
       tags (family/stage/agent/box/run_ts from harbor-launch.sh ``pk_meta``),
    3. the ``STAFF_BENCH_*`` host env (family/repo/run_ts/box, set on stage
       containers by the pipeline),
    4. ``component`` + caller ``extra``.

    Falsy values are dropped. Callers attach the result ONLY when Portkey
    routing is active — a direct-provider request would 400 on an unknown
    ``x-portkey-*`` header. Keep the schema in sync with
    ``task_maker/agent_server.py::_append_portkey_attribution``, which is baked
    standalone into the agent image and cannot import this module (its phase
    stays the ``task-gen`` literal — the stage agent only runs in task-gen).
    """
    phase = phase or os.environ.get("STAFF_BENCH_PHASE") or "task-gen"
    meta: dict[str, str] = {"engagement_name": "senior-swe-bench", "phase": phase}
    inherited = os.environ.get("SSB_PORTKEY_METADATA")
    if inherited:
        try:
            parsed = json.loads(inherited)
        except json.JSONDecodeError:
            parsed = None
        if isinstance(parsed, dict):
            meta.update({str(k): str(v) for k, v in parsed.items()})
    env_fields = {
        "family": os.environ.get("STAFF_BENCH_FAMILY", ""),
        "repo": os.environ.get("STAFF_BENCH_REPO", "") or os.environ.get("REPO_NAME", ""),
        "run_ts": os.environ.get("STAFF_BENCH_RUN_TS", ""),
        "box": os.environ.get("STAFF_BENCH_BOX", ""),
        # Binary segment: "fix" (taxonomy bug/performance) or "feature"
        # (feature/migration) — producers map via fix_or_feature() and export.
        "task_type": os.environ.get("STAFF_BENCH_TASK_TYPE", ""),
        # Pipeline-stage label (final-grading exports per-stage: screen/smoke/
        # difficulty/rollout-audit/backfill/grade). Callers may still override
        # via extra (audit workers pass their own stage explicitly).
        "stage": os.environ.get("STAFF_BENCH_STAGE", ""),
    }
    meta.update({k: v for k, v in env_fields.items() if v})
    meta["component"] = component
    if extra:
        meta.update({str(k): str(v) for k, v in extra.items() if v})
    return json.dumps({k: v for k, v in meta.items() if v}, sort_keys=True)


def have_credentials() -> bool:
    """True if any usable credential is present: gateway (Portkey), or a direct
    provider key (Anthropic or OpenAI). Direct OpenAI applies when the SSB_OVERRIDE_*
    model slugs point at ``openai/...`` and only OPENAI_API_KEY is set."""
    return bool(resolve_portkey_key() or os.environ.get("ANTHROPIC_API_KEY") or os.environ.get("OPENAI_API_KEY"))


def judge_model() -> str:
    """Judge model (rubric, taste, validation review): ``SSB_OVERRIDE_ALL_JUDGE_MODEL`` if set, else the default."""
    return os.environ.get(JUDGE_MODEL_OVERRIDE_VAR) or JUDGE_MODEL_DEFAULT


def classifier_model() -> str:
    """Patch-classifier model: ``SSB_OVERRIDE_CLASSIFIER_MODEL`` if set, else the default."""
    return os.environ.get(CLASSIFIER_MODEL_OVERRIDE_VAR) or CLASSIFIER_MODEL_DEFAULT


def judge_panel() -> list[str]:
    """Rubric + taste judge panel: ``SSB_OVERRIDE_TASTE_RUBRIC_PANEL`` (comma-separated) if
    set, else ``[judge_model()]``. Callers average per-dimension scores across the models."""
    raw = os.environ.get(PANEL_MODEL_OVERRIDE_VAR)
    if raw:
        models = [m.strip() for m in raw.split(",") if m.strip()]
        if models:
            return models
    return [judge_model()]


# ───────────────────────────────────────────────────────────────────────────
# litellm routing + call helpers
# ───────────────────────────────────────────────────────────────────────────


def _litellm() -> Any:
    """Import litellm lazily; tolerate provider-unsupported params (e.g. thinking)."""
    import litellm

    litellm.drop_params = True
    return litellm


@dataclasses.dataclass(frozen=True)
class Routing:
    """litellm.completion kwargs for one call, resolved from the environment.

    ``cache_control`` is not a litellm kwarg. It is a flag ``complete()`` reads to
    decide whether to inject prompt-cache markers, set when the model is an
    Anthropic one. Markers are useless (and may be rejected) elsewhere, so it is
    never set for other models.
    """

    model: str
    extra_headers: dict[str, str] | None = None
    cache_control: bool = False

    def completion_kwargs(self) -> dict[str, Any]:
        kw: dict[str, Any] = {"model": self.model}
        if self.extra_headers:
            kw["extra_headers"] = dict(self.extra_headers)
        return kw


def is_anthropic_model(model: str) -> bool:
    """True if the slug names a Claude/Anthropic model.

    The one thing routing branches on. Mirrored by mswea_config.py so the judge
    and solver never disagree.
    """
    m = model.lower()
    return "claude" in m or "anthropic" in m


def litellm_slug(model: str) -> str:
    """The slug litellm gets, for a configured ``<provider>/<model>``.

    Exactly one provider is ever configured, and it is either a real litellm
    provider (``anthropic/claude-sonnet-4-6``) or a Portkey ``@``-catalog one
    (``@bedrock/global.anthropic.claude-sonnet-4-6``). litellm reads everything
    before the first ``/`` as ITS provider, and ``@bedrock`` is not one, so a
    bare catalog slug dies with ``BadRequestError("LLM Provider NOT provided")``
    before a request is ever sent. Nest it under the litellm provider that
    should carry it:

    * ``anthropic`` for Claude models -- its ``/v1/messages`` route is the only
      one that preserves prompt-cache markers. Probed 2026-08-20 on mini-swe's
      own message shape: 3090 cache-read tokens through the anthropic provider,
      0 through the OpenAI-compatible one, which drops the marker mini-swe puts
      on the trailing tool message.
    * ``openai`` for everything else, where caching is implicit or absent.

    litellm then consumes the segment it added and passes the rest through as an
    opaque model name, so a catalog slug survives intact. Splitting is never
    needed, which is why a model half containing slashes
    (``@openrouter/moonshotai/kimi-k3``) needs no special case.

    Internal output only. The two-provider form is never written in config.
    """
    if not model.startswith("@"):
        return model
    return ("anthropic/" if is_anthropic_model(model) else "openai/") + model


def _gateway_attribution() -> dict[str, str]:
    """Portkey dashboard attribution headers for a gateway route.

    Metadata is ALWAYS attached: the launch env when set, else assembled locally
    — so a judge or classifier call can never ship untagged (portkey_metadata_json
    inherits SSB_PORTKEY_METADATA itself, making the two branches agree whenever
    the env is present; reading the env directly is kept for exactness and
    cheapness on the common path). Trace id comes only from the launch layer
    (harbor-launch.sh exports; task.toml [verifier.env] interpolates it into the
    Modal sandbox).
    """
    headers: dict[str, str] = {}
    if os.environ.get("SSB_PORTKEY_TRACE_ID"):
        headers["x-portkey-trace-id"] = os.environ["SSB_PORTKEY_TRACE_ID"]
    if os.environ.get("SSB_PORTKEY_METADATA"):
        headers["x-portkey-metadata"] = os.environ["SSB_PORTKEY_METADATA"]
    else:
        headers["x-portkey-metadata"] = portkey_metadata_json("judge")
    return headers


def _require_gateway_endpoint(model: str) -> None:
    """Fail loudly when a Portkey key is set but this model's endpoint var is not.

    litellm reads the endpoint from the environment, and with the var absent it
    silently falls back to the vendor's own API -- where the gateway key is not a
    valid credential. On 2026-08-20 that surfaced as ``invalid x-api-key`` from
    api.anthropic.com inside a Modal verifier sandbox (whose env is only what
    task.toml [verifier.env] declares, not the launcher's), and it cost a full run
    to trace back from "validation agent infrastructure failure". Cheaper to refuse.
    """
    var = "ANTHROPIC_BASE_URL" if is_anthropic_model(model) else "OPENAI_BASE_URL"
    if not os.environ.get(var):
        raise RuntimeError(
            f"{var} is unset but PORTKEY_API_KEY is set: litellm would send the gateway "
            f"key to the vendor's own API and 401. Set it in the environment (.secrets on "
            f"a host; task.toml [verifier.env] carries it into the verifier sandbox)."
        )


def resolve_routing(model: str) -> Routing:
    """litellm kwargs for one judge call.

    Endpoint and credential come from the environment -- ``ANTHROPIC_BASE_URL`` /
    ``ANTHROPIC_AUTH_TOKEN`` and ``OPENAI_BASE_URL`` / ``OPENAI_API_KEY``, which
    litellm reads for itself (main.py:2594, :1591). No endpoint or key is
    configured here, so repointing the repo is an env change, not a code change.

    The ``x-portkey-api-key`` header IS required: Portkey's ``/v1/messages``
    passthrough rejects the Bearer credential litellm sends for Anthropic models
    (probed 2026-08-20 in isolated processes -- an earlier probe that suggested
    otherwise was contaminated by litellm's client cache). It is harmless on the
    OpenAI-compatible route, which authenticates by Bearer, so it is attached
    unconditionally rather than branched on the model.

    Two things are decided: an Anthropic model carries cache_control markers, and
    a Portkey key adds auth + attribution headers. Slugs pass through verbatim.
    """
    key = resolve_portkey_key()
    slug = litellm_slug(model)
    if key:
        # Checked on the synthesized slug, not the configured one: the check
        # decides which endpoint var litellm will read, so it should reason
        # about the exact string litellm receives.
        _require_gateway_endpoint(slug)
    headers = {"x-portkey-api-key": key, **_gateway_attribution()} if key else None
    return Routing(model=slug, extra_headers=headers, cache_control=is_anthropic_model(slug))


def _with_trailing_cache_breakpoint(msgs: list[dict[str, Any]]) -> list[dict[str, Any]]:
    """Return ``msgs`` with an ephemeral cache breakpoint on the last message.

    A shallow copy of the final message carries a top-level ``cache_control``
    key, which litellm's anthropic provider reads off user/tool/assistant string
    content and stamps onto the corresponding Messages block — caching the whole
    prefix up to that point. In the multi-turn explore loop the prefix grows each
    turn, so the trailing breakpoint (plus the system one) keeps the reused head
    served from cache. The caller's message dicts are never mutated.
    """
    tail = {**msgs[-1], "cache_control": {"type": "ephemeral"}}
    return [*msgs[:-1], tail]


def _is_openai_reasoning(model: str) -> bool:
    """True for OpenAI reasoning slugs (``openai/gpt-5.x``, ``openai/o<N>``); matches run_validate.py."""
    leaf = model.rsplit("/", maxsplit=1)[-1].lower()
    return model.startswith("openai/") and (leaf.startswith("gpt-5") or (leaf[:1] == "o" and leaf[1:2].isdigit()))


def openai_tool(name: str, description: str, parameters: dict[str, Any]) -> dict[str, Any]:
    """Build an OpenAI-format tool entry from a JSON-schema ``parameters`` dict."""
    return {"type": "function", "function": {"name": name, "description": description, "parameters": parameters}}


def tool_call_args(response: Any, name: str | None = None) -> dict[str, Any] | None:
    """Parsed JSON arguments of the first tool call (optionally matching ``name``).

    Returns ``None`` when no matching tool call is present or its arguments
    aren't valid JSON.
    """
    try:
        message = response.choices[0].message
    except (AttributeError, IndexError):
        return None
    for call in getattr(message, "tool_calls", None) or []:
        fn = getattr(call, "function", None)
        if fn is None:
            continue
        if name is None or fn.name == name:
            try:
                return json.loads(fn.arguments)
            except (TypeError, json.JSONDecodeError):
                return None
    return None


def complete(  # noqa: PLR0913
    *,
    model: str,
    messages: list[dict[str, Any]],
    max_tokens: int,
    tools: list[dict[str, Any]] | None = None,
    tool_choice: Any = None,
    system: str | None = None,
    thinking_budget: int = 0,
    attempts: int = _RETRY_ATTEMPTS,
    **kwargs: Any,
) -> Any:
    """One model call via ``litellm.completion`` with bounded transient retry.

    ``system`` is prepended as an OpenAI ``system`` message. ``thinking_budget``
    enables extended thinking where the provider supports it (dropped otherwise,
    via ``litellm.drop_params``).

    On the anthropic gateway route (``routing.cache_control``) two ephemeral
    cache breakpoints are injected so the shared prefix is served from Anthropic's
    prompt cache instead of re-billed per call: one on the ``system`` block
    (constant across a judge/classifier run) and, for a multi-turn thread, one on
    the last message (the growing explore history — each turn extends the prefix
    the prior breakpoint already cached). litellm's anthropic provider reads these
    markers off the structured system block and message content and translates
    them onto the Messages wire format; on every other route the markers are a
    no-op that we deliberately never attach (a non-Anthropic route would drop or
    reject them).
    """
    litellm = _litellm()
    routing = resolve_routing(model)

    msgs = list(messages)
    if system is not None:
        if routing.cache_control:
            # Structured system block so litellm's anthropic provider carries the
            # cache_control marker (a plain string system message cannot hold one).
            msgs = [
                {
                    "role": "system",
                    "content": [{"type": "text", "text": system, "cache_control": {"type": "ephemeral"}}],
                },
                *msgs,
            ]
        else:
            msgs = [{"role": "system", "content": system}, *msgs]
    if routing.cache_control and msgs:
        msgs = _with_trailing_cache_breakpoint(msgs)

    call_kwargs: dict[str, Any] = {**routing.completion_kwargs(), "messages": msgs, "max_tokens": max_tokens}
    if tools is not None:
        call_kwargs["tools"] = tools
    if tool_choice is not None:
        call_kwargs["tool_choice"] = tool_choice
    if thinking_budget > 0:
        call_kwargs["thinking"] = {"type": "enabled", "budget_tokens": thinking_budget}
    call_kwargs.update(kwargs)

    # gpt-5.x: set reasoning_effort so litellm bridges to the Responses API (chat-shaped
    # output, so callers are unchanged). Other models untouched.
    if _is_openai_reasoning(routing.model) and "reasoning_effort" not in call_kwargs:
        call_kwargs["reasoning_effort"] = _OPENAI_REASONING_EFFORT

    resp = _call_with_retry(litellm.completion, attempts, call_kwargs)
    # Re-issue a forced-tool call that returned no parseable tool OR empty
    # arguments (`{}` — typically output truncation; it satisfies "parseable"
    # but can never validate downstream). Bounded; a no-op when the first call
    # already returns a non-empty tool payload.
    forced = tool_choice.get("function", {}).get("name") if isinstance(tool_choice, dict) else None
    if forced:
        tries = 0
        while not tool_call_args(resp, forced) and tries < _FORCED_TOOL_RETRIES:
            tries += 1
            resp = _call_with_retry(litellm.completion, attempts, call_kwargs)
    return resp


def complete_structured(  # noqa: PLR0913
    *,
    model: str,
    messages: list[dict[str, Any]],
    schema: type[BaseModel],
    max_tokens: int = 2048,
    system: str | None = None,
    thinking_budget: int = 0,
    tool_name: str = "emit_result",
    tool_description: str = "Emit the result as structured data.",
    attempts: int = _RETRY_ATTEMPTS,
) -> Any | None:
    """Forced structured output: one forced tool call parsed into ``schema``.

    Returns a validated ``schema`` instance, or ``None`` if the model produced
    no parseable tool call (caller decides the fallback). Forced-tool is used
    rather than ``response_format`` for the broadest cross-provider support.
    """
    tool = openai_tool(tool_name, tool_description, schema.model_json_schema())
    response = complete(
        model=model,
        messages=messages,
        max_tokens=max_tokens,
        tools=[tool],
        tool_choice={"type": "function", "function": {"name": tool_name}},
        system=system,
        thinking_budget=thinking_budget,
        attempts=attempts,
    )
    args = tool_call_args(response, tool_name)
    # Falsy covers both "no tool call" and empty `{}` arguments — Anthropic
    # returns tool input as a parsed object, so output truncation surfaces as
    # a clean `{}` (never cut JSON) that would raise from model_validate.
    if not args:
        return None
    return schema.model_validate(args)


# The validation agent is a subprocess (Claude Code CLI or mini-swe-agent), not
# an in-process litellm call, so it is routed via environment variables rather
# than resolve_routing(). agent_routing() is the single place that knows how each
# harness reaches the gateway; callers apply the returned env + model fields blindly.


@dataclasses.dataclass
class AgentRouting:
    """Routing config for a validation-agent subprocess.

    ``env`` is merged into the subprocess environment; ``model`` is the slug to
    invoke with; ``extra_headers`` are per-request gateway headers the harness must
    thread into its model config (mini-swe-agent only — the claude_code harness
    carries them inside ``env["ANTHROPIC_CUSTOM_HEADERS"]``). ``label`` is a
    human tag for logs ("gateway" / "direct").
    """

    model: str
    env: dict[str, str]
    label: str = "direct"
    extra_headers: dict[str, str] = dataclasses.field(default_factory=dict)
    cache_control: bool = False


def agent_routing(harness: str, model: str) -> AgentRouting:
    """Resolve subprocess routing for a validation-agent ``harness`` + ``model``.

    ``harness`` is ``"claude_code"`` (routed via ANTHROPIC_* env vars) or
    ``"miniswe"`` (routed via PORTKEY_API_KEY + litellm model_class/provider).
    With no routing key set, returns direct config (empty routing env — no
    ``x-portkey-*`` header is ever attached on the direct path).

    Gateway routes also carry Portkey dashboard attribution
    (``component=validation-agent`` metadata + the inherited trace id) so VA
    spend is filterable alongside judges/trials rather than landing untagged.
    """
    key = resolve_portkey_key()
    if not key:
        return AgentRouting(model=model, env={}, cache_control=is_anthropic_model(model))

    attribution = {"x-portkey-metadata": portkey_metadata_json("validation-agent")}
    if os.environ.get("SSB_PORTKEY_TRACE_ID"):
        attribution["x-portkey-trace-id"] = os.environ["SSB_PORTKEY_TRACE_ID"]

    if harness == "claude_code":
        headers = f"x-portkey-api-key: {key}" + "".join(f"\n{name}: {value}" for name, value in attribution.items())
        return AgentRouting(
            model=model,
            env={
                "ANTHROPIC_BASE_URL": _PORTKEY_BASE_URL,
                "ANTHROPIC_AUTH_TOKEN": key,
                "ANTHROPIC_CUSTOM_HEADERS": headers,
            },
            label="gateway",
        )

    # mini-swe (litellm): endpoint + credential come from the container env, same
    # as the judges. The x-portkey-api-key header is still required for Anthropic
    # models (Portkey rejects litellm's Bearer there) and harmless elsewhere.
    slug = litellm_slug(model)
    _require_gateway_endpoint(slug)
    return AgentRouting(
        model=slug,
        env={"PORTKEY_API_KEY": key},
        label="gateway",
        extra_headers={**attribution, "x-portkey-api-key": key},
        cache_control=is_anthropic_model(slug),
    )


def _is_transient(exc: Exception) -> bool:
    code = getattr(exc, "status_code", None)
    if code is not None:
        try:
            if int(code) in _TRANSIENT_STATUS_CODES:
                return True
        except (TypeError, ValueError):
            pass
    msg = str(exc).lower()
    return any(marker in msg for marker in _TRANSIENT_MARKERS)


def _call_with_retry(fn: Any, attempts: int, kwargs: dict) -> Any:
    """Call ``fn(**kwargs)`` with bounded retry on transient errors.

    Retries up to ``attempts`` times with exponential backoff (1, 2, 4, …
    capped at 30s); non-transient errors raise immediately. If every attempt
    fails the exception propagates.
    """
    last_exc: Exception | None = None
    for attempt in range(attempts):
        try:
            return fn(**kwargs)
        except Exception as e:  # noqa: BLE001 — classify by message; SDK type is Any
            last_exc = e
            if attempt == attempts - 1 or not _is_transient(e):
                raise
            time.sleep(min(2**attempt, 30))
    # Unreachable: the loop returns on success or raises on the final attempt. The
    # fallback keeps the type non-None (mypy) without asserting (bandit S101).
    raise last_exc or RuntimeError("retry loop exited without a result")  # pragma: no cover
