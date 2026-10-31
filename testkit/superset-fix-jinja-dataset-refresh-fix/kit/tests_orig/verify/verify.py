"""Behavioral verifier for superset-fix-jinja-dataset-refresh.

The symptom: saving a virtual dataset whose SQL contains Jinja template
blocks pops an "Invalid SQL" error toast even though the row was already
persisted. The save goes through the dataset editor's PUT to the REST API;
after the row is committed, a post-save column-metadata refresh re-templates
and re-parses the stored SQL with no query runtime context, and today treats
every template/parse failure as fatal, so the whole request fails. The fix
must make that refresh best-effort for the "no runtime context" failures ONLY,
while leaving every other failure on the same path hard-failing exactly as
before.

HOW THIS VERIFIER STAYS IMPLEMENTATION-AGNOSTIC

Every scenario is exercised through the SAME public interface the dataset
editor uses: the HTTP endpoint

    PUT /api/v1/dataset/<pk>?override_columns=true

driven with Flask's test client against a real, migrated metadata DB and a
logged-in admin. We assert only the user-observable HTTP outcome:

    best-effort  -> 200, and the row's edit persisted
    hard failure -> a client/server error status (>= 400), row still intact

This is the highest-level stable interface the symptom is observable through,
so it admits EVERY valid fix locus without caring where the softening lives:
the virtual-table metadata function, the refresh command, the put() handler
that calls the command, or a combination. A command-level fix (catch a narrow
template/parse failure inside RefreshDatasetCommand.run()), a metadata-function
fix, and a handler-level fix (let the generic DB error propagate out of run()
and discriminate it in put() by walking the exception cause chain) all deliver
the same observable matrix and all pass here. We never import or name any new
exception subclass, marker helper, or wording the fix introduces.

The discrimination matrix (unchanged from the symptom analysis):

    undefined-variable template render failure       -> best-effort (200)
    Jinja-marked SQL that renders to unparseable SQL -> best-effort (200)
    plain static SQL that fails to parse             -> hard error (>= 400)
    sandbox/security violation during templating     -> hard error (>= 400)
    genuine driver/connection DB error on refresh    -> hard error (>= 400)
    plain, valid SQL                                 -> success (200)

Two of these failures (the undefined-variable render and the sandbox/security
violation) cannot be provoked by real rendering here, because with no feature
flags the template processor renders undefined markers permissively. For those
two rows ONLY we inject the failure at the pre-existing template-processor seam
(SqlaTable.get_template_processor) exactly as Superset's own tests do, carrying
the real jinja2 cause on __cause__ so a cause-based discrimination can act on
it. The remaining rows use real SQL through the real parse path with no
injection. The genuine-DB-error row injects at the pre-existing
SqlaTable.fetch_metadata seam. No seam here is a symbol the solver had to
invent.
"""

from __future__ import annotations

import os
import uuid
from unittest.mock import Mock, patch

import pytest
from jinja2 import UndefinedError
from jinja2.exceptions import SecurityError

# NOTE on import placement: superset.models.core (reached transitively by the
# symbols below) builds encrypted-field columns at class-definition time and
# raises "App not initialized yet" unless an app has already run init_app().
# Importing those symbols at module scope would fire that failure during pytest
# COLLECTION, before any fixture runs. So every superset symbol is imported
# lazily inside the app fixture or the helpers — never at module scope. This
# mirrors the repo's own test suite, whose conftest initializes an app before
# any model module is imported.

GET_TEMPLATE_PROCESSOR = (
    "superset.connectors.sqla.models.SqlaTable.get_template_processor"
)
FETCH_METADATA = "superset.connectors.sqla.models.SqlaTable.fetch_metadata"


# ---------------------------------------------------------------------------
# Application: a real SupersetApp with a migrated, file-backed SQLite metadata
# DB, FAB roles synced, and an admin user — the minimum needed to drive the
# dataset REST API through the test client exactly as the dataset editor does.
# This mirrors the repo's integration-test bootstrap (db upgrade + init +
# load-test-users) but stays in-process and offline.
# ---------------------------------------------------------------------------
@pytest.fixture(scope="module")
def app(tmp_path_factory):
    db_file = tmp_path_factory.mktemp("meta") / "superset.db"
    uri = os.environ.get("SUPERSET__SQLALCHEMY_DATABASE_URI") or f"sqlite:///{db_file}"

    from superset.app import SupersetApp
    from superset.extensions import appbuilder
    from superset.initialization import SupersetAppInitializer

    application = SupersetApp(__name__)
    application.config.from_object("superset.config")
    application.config["SQLALCHEMY_DATABASE_URI"] = uri
    application.config["SECRET_KEY"] = "verifier-secret-key-" + "x" * 24
    application.config["WTF_CSRF_ENABLED"] = False
    application.config["PREVENT_UNSAFE_DB_CONNECTIONS"] = False
    application.config["TESTING"] = True
    application.config["RATELIMIT_ENABLED"] = False
    application.config["CACHE_CONFIG"] = {}
    application.config["DATA_CACHE_CONFIG"] = {}
    application.config["SERVER_NAME"] = "example.com"
    application.config["APPLICATION_ROOT"] = "/"
    application.config["PREFERRED_URL_SCHEME"] = "http"
    # Skip the unrelated post-fetch datetime-format detection block in
    # RefreshDatasetCommand.run() so the tests isolate the metadata-refresh
    # behavior under test. SQL_MAX_PARSE_LENGTH keeps its superset.config
    # default so the parser exercises genuine syntax handling.
    application.config["DATASET_AUTO_DETECT_DATETIME_FORMATS"] = False

    # appbuilder is a singleton; clear registered views so init can re-run.
    appbuilder.baseviews = []
    SupersetAppInitializer(application).init_app()

    with application.app_context():
        import superset
        from flask_migrate import upgrade as db_upgrade

        migrations_dir = os.path.join(os.path.dirname(superset.__file__), "migrations")
        db_upgrade(directory=migrations_dir)

        # Sync FAB permissions/roles and create an admin, mirroring
        # `superset init` + `superset load-test-users`.
        appbuilder.add_permissions(update_perms=True)
        appbuilder.sm.sync_role_definitions()

        from superset import db, security_manager

        admin_role = security_manager.find_role("Admin")
        if security_manager.find_user("admin") is None:
            security_manager.add_user(
                "admin", "Admin", "User", "admin@example.com", admin_role,
                password="general",
            )
            db.session.commit()

    return application


@pytest.fixture
def client(app):
    """A logged-in admin test client inside a fresh app context."""
    with app.app_context():
        c = app.test_client()
        resp = c.post(
            "/login/", data={"username": "admin", "password": "general"}
        )
        assert resp.status_code in (200, 302), (
            f"admin login failed: {resp.status_code}"
        )
        yield c


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
def _make_virtual_dataset(sql):
    """Persist a virtual dataset with the given SQL and return its pk."""
    from superset import db
    from superset.connectors.sqla.models import SqlaTable
    from superset.models.core import Database

    database = db.session.query(Database).filter_by(database_name="verifier_db").first()
    if database is None:
        database = Database(
            database_name="verifier_db", sqlalchemy_uri="sqlite://"
        )
        db.session.add(database)
        db.session.commit()

    table = SqlaTable(
        table_name="virt_" + uuid.uuid4().hex[:8],
        database=database,
        sql=sql,
    )
    db.session.add(table)
    db.session.commit()
    return table.id


def _save(client, pk, *, description):
    """Issue the dataset editor's save: PUT with override_columns, which runs
    the post-save column-metadata refresh."""
    return client.put(
        f"/api/v1/dataset/{pk}?override_columns=true",
        json={"description": description},
    )


def _persisted_description(pk):
    from superset import db
    from superset.connectors.sqla.models import SqlaTable

    db.session.expire_all()
    return db.session.query(SqlaTable).get(pk).description


def _syntax_error_with_cause(cause):
    """Build the wrapper SupersetSyntaxErrorException exactly as the template
    processor does, carrying the original jinja2 error on __cause__."""
    from superset.errors import ErrorLevel, SupersetError, SupersetErrorType
    from superset.exceptions import SupersetSyntaxErrorException

    ex = SupersetSyntaxErrorException(
        [
            SupersetError(
                message="Jinja2 template error",
                error_type=SupersetErrorType.GENERIC_COMMAND_ERROR,
                level=ErrorLevel.ERROR,
            )
        ]
    )
    ex.__cause__ = cause
    return ex


def _processor_raising(error):
    """A template processor whose process_template raises ``error`` — the
    pre-existing seam Superset's own tests stub to force a template failure."""
    processor = Mock()
    processor.process_template.side_effect = error
    return processor


# ---------------------------------------------------------------------------
# Best-effort rows: the save must succeed (200) and persist the edit.
# On the unmodified tree the post-save refresh hard-fails, so the PUT errors
# (>= 400) and these tests fail — sound nop-gating.
# ---------------------------------------------------------------------------
def test_save_is_best_effort_on_undefined_template_variable(client):
    """Saving a Jinja dataset whose template cannot render because a template
    variable is undefined (no runtime context at save time) must succeed: the
    editor's PUT returns 200 and the edit persists, with no spurious error."""
    pk = _make_virtual_dataset("SELECT '{{ from_dttm.isoformat() }}'")
    with patch(
        GET_TEMPLATE_PROCESSOR,
        return_value=_processor_raising(
            _syntax_error_with_cause(UndefinedError("'from_dttm' is undefined"))
        ),
    ):
        resp = _save(client, pk, description="undef-save")
    assert resp.status_code == 200, (
        "saving a Jinja dataset that fails to render for lack of runtime "
        f"context must be best-effort (HTTP 200), got {resp.status_code}: "
        f"{resp.data[:300]!r}"
    )
    assert _persisted_description(pk) == "undef-save"


def test_save_is_best_effort_on_jinja_marked_parse_failure(client):
    """Saving a dataset whose SQL contains Jinja markers and renders (with
    empty runtime context) to SQL the parser rejects must also succeed — the
    SQL only resolves at query time."""
    pk = _make_virtual_dataset("SELECT INVALID SYNTAX FROM {{ some_table }}")
    resp = _save(client, pk, description="jinja-parse-save")
    assert resp.status_code == 200, (
        "saving a Jinja-templated dataset whose rendered SQL fails to parse "
        f"must be best-effort (HTTP 200), got {resp.status_code}: "
        f"{resp.data[:300]!r}"
    )
    assert _persisted_description(pk) == "jinja-parse-save"


# ---------------------------------------------------------------------------
# Hard-failure rows: the save must FAIL (>= 400). They guard against over-broad
# fixes (swallow everything / sniff the SQL text / soften by SQL shape). They
# fail on both the unmodified tree and a correct fix; a too-aggressive fix
# flips one of them from erroring to 200.
# ---------------------------------------------------------------------------
def test_save_still_hard_errors_on_static_sql_parse_failure(client):
    """A parse failure on plain static SQL (no Jinja markers) is a genuine
    defect and the save must still hard-error. A fix that softens all parse
    failures would let this return 200."""
    pk = _make_virtual_dataset("SELECT INVALID SYNTAX FROM")
    resp = _save(client, pk, description="static-bad")
    assert resp.status_code >= 400, (
        "saving a plain-SQL dataset with genuinely invalid SQL must still "
        f"hard-error, got {resp.status_code}: {resp.data[:300]!r}"
    )


def test_save_still_hard_errors_on_template_security_violation(client):
    """A template failure caused by a sandbox/security violation (not a missing
    variable) must still hard-error — softening it would silence a real
    security-policy failure. The decision keys off what failed (the wrapped
    cause), not the SQL text."""
    pk = _make_virtual_dataset("SELECT {{ some_disallowed_attr }}")
    with patch(
        GET_TEMPLATE_PROCESSOR,
        return_value=_processor_raising(
            _syntax_error_with_cause(SecurityError("access to attribute is unsafe"))
        ),
    ):
        resp = _save(client, pk, description="sec-violation")
    assert resp.status_code >= 400, (
        "a sandbox/security violation during templating must still hard-error "
        f"on save, got {resp.status_code}: {resp.data[:300]!r}"
    )


def test_save_still_hard_errors_on_genuine_db_error(client):
    """A genuine generic DB error raised while refreshing (connection / driver
    / permission failure surfaced by the column-description path) must still
    hard-error — even when the dataset's SQL also contains Jinja markers.
    Keying softening off the SQL text would wrongly swallow this."""
    from superset.exceptions import SupersetGenericDBErrorException

    pk = _make_virtual_dataset(
        "SELECT * FROM t {% if from_dttm %}WHERE ds > '{{ from_dttm }}'{% endif %}"
    )
    with patch(
        FETCH_METADATA,
        side_effect=SupersetGenericDBErrorException(message="Connection refused"),
    ):
        resp = _save(client, pk, description="db-error")
    assert resp.status_code >= 400, (
        "a genuine DB error during the post-save refresh must still hard-error "
        f"even on a Jinja dataset, got {resp.status_code}: {resp.data[:300]!r}"
    )


# ---------------------------------------------------------------------------
# Unaffected paths: plain valid SQL saves cleanly; validation paths unchanged.
# ---------------------------------------------------------------------------
def test_save_succeeds_on_plain_valid_sql(client):
    """A plain, valid virtual-dataset SQL still saves cleanly (200). Guards
    against a fix that breaks the happy path."""
    pk = _make_virtual_dataset("SELECT 1 AS a")
    resp = _save(client, pk, description="plain-ok")
    assert resp.status_code == 200, (
        f"saving a plain valid dataset must succeed, got {resp.status_code}: "
        f"{resp.data[:300]!r}"
    )
    assert _persisted_description(pk) == "plain-ok"


def test_save_on_unknown_dataset_returns_not_found(client):
    """Saving an unknown dataset id must still return 404; the best-effort
    change must not mask validation."""
    resp = _save(client, 999_999, description="nope")
    assert resp.status_code == 404, (
        f"unknown dataset id must return 404, got {resp.status_code}: "
        f"{resp.data[:300]!r}"
    )
