"""Behavioral checks for saving virtual datasets whose SQL uses Jinja.

Each scenario saves a virtual dataset the way the dataset editor does,

    PUT /api/v1/dataset/<pk>?override_columns=true

through Flask's test client, against a migrated metadata DB and a logged-in
admin, and checks the HTTP outcome:

    best-effort  -> 200, the edit is persisted, and the dataset keeps the
                    columns it had before the save
    hard failure -> an error status (>= 400)

Expected outcomes:

    undefined template variable during rendering     -> best-effort (200)
    Jinja-marked SQL that renders to unparseable SQL -> best-effort (200)
    plain static SQL that fails to parse             -> hard error (>= 400)
    sandbox/security violation during templating     -> hard error (>= 400)
    generic DB error during the save                 -> hard error (>= 400)
    plain, valid SQL                                 -> success (200)
    unknown dataset id                               -> 404

The undefined variable row turns on ENABLE_TEMPLATE_PROCESSING for that save
only, so the SQL goes through Superset's real Jinja processor. The sandbox
row patches the existing SqlaTable.get_template_processor seam and raises a
jinja2 SecurityError both bare and wrapped in SupersetSyntaxErrorException,
the two forms a template error can take in this codebase. The generic DB
error row patches the existing SqlaTable.fetch_metadata seam. The parse rows
use real SQL with no patching.
"""

from __future__ import annotations

import os
import uuid
from unittest.mock import Mock, patch

import pytest
from jinja2.exceptions import SecurityError

# Import placement: superset.models.core (pulled in by the symbols below)
# builds encrypted-field columns at class-definition time and raises "App not
# initialized yet" unless init_app() has already run. Importing those symbols
# at module scope would fail during pytest collection, so every superset symbol
# is imported inside the app fixture or the helpers. The repo's own conftest
# does the same by creating an app before any model module is imported.

GET_TEMPLATE_PROCESSOR = (
    "superset.connectors.sqla.models.SqlaTable.get_template_processor"
)
FETCH_METADATA = "superset.connectors.sqla.models.SqlaTable.fetch_metadata"


# ---------------------------------------------------------------------------
# Application: a SupersetApp with a migrated, file-backed SQLite metadata DB,
# FAB roles synced, and an admin user. That is the minimum needed to call the
# dataset REST API through the test client the way the dataset editor does.
# It follows the repo's integration-test bootstrap (db upgrade + init +
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
    # Turn off datetime-format detection on save. It is unrelated to these
    # checks and only adds noise. SQL_MAX_PARSE_LENGTH keeps its superset.config
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
# Physical columns a saved dataset already has. A best-effort save must leave
# them in place.
EXISTING_COLUMNS = ("ds", "amount")


def _make_virtual_dataset(sql, columns=()):
    """Persist a virtual dataset with the given SQL and physical columns and
    return its pk."""
    from superset import db
    from superset.connectors.sqla.models import SqlaTable, TableColumn
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
    table.columns = [TableColumn(column_name=name, type="VARCHAR") for name in columns]
    db.session.add(table)
    db.session.commit()
    return table.id


def _save(client, pk, *, description):
    """Issue the dataset editor's save: PUT with override_columns=true."""
    return client.put(
        f"/api/v1/dataset/{pk}?override_columns=true",
        json={"description": description},
    )


def _persisted_description(pk):
    from superset import db
    from superset.connectors.sqla.models import SqlaTable

    db.session.expire_all()
    return db.session.query(SqlaTable).get(pk).description


def _persisted_column_names(pk):
    from superset import db
    from superset.connectors.sqla.models import TableColumn

    db.session.expire_all()
    rows = db.session.query(TableColumn.column_name).filter_by(table_id=pk).all()
    return sorted(name for (name,) in rows)


def _assert_best_effort_save(pk, resp, description, what):
    assert resp.status_code == 200, (
        f"saving {what} must be best-effort (HTTP 200), got "
        f"{resp.status_code}: {resp.data[:300]!r}"
    )
    assert _persisted_description(pk) == description, (
        f"saving {what} must persist the edit"
    )
    assert _persisted_column_names(pk) == sorted(EXISTING_COLUMNS), (
        f"saving {what} must keep the dataset's existing columns "
        f"{sorted(EXISTING_COLUMNS)}, found {_persisted_column_names(pk)}"
    )


def _template_processing_enabled():
    """Turn on ENABLE_TEMPLATE_PROCESSING without touching other flags."""
    from superset.extensions import feature_flag_manager

    real = feature_flag_manager.is_feature_enabled
    return patch.object(
        feature_flag_manager,
        "is_feature_enabled",
        side_effect=lambda f: f == "ENABLE_TEMPLATE_PROCESSING" or real(f),
    )


def _syntax_error_with_cause(cause, sql):
    """Build the SupersetSyntaxErrorException that process_template() raises
    for a jinja2 error: same message format, same extra fields, and the
    original error on __cause__."""
    from superset.errors import ErrorLevel, SupersetError, SupersetErrorType
    from superset.exceptions import SupersetSyntaxErrorException

    exception_type = type(cause).__name__
    ex = SupersetSyntaxErrorException(
        [
            SupersetError(
                message=f"Jinja2 template error ({exception_type}): {cause}",
                error_type=SupersetErrorType.GENERIC_COMMAND_ERROR,
                level=ErrorLevel.ERROR,
                extra={
                    "template": sql[:500],
                    "line": getattr(cause, "lineno", None),
                    "exception_type": exception_type,
                },
            )
        ]
    )
    ex.__cause__ = cause
    return ex


def _processor_raising(error):
    """A template processor whose process_template() raises ``error``. This is
    the same seam Superset's own tests stub to force a template failure."""
    processor = Mock()
    processor.process_template.side_effect = error
    return processor


def _template_error_shapes(make_error, sql):
    """The two shapes a jinja2 error takes when it leaves process_template():
    wrapped (compile stage) and unwrapped (render stage)."""
    return (
        ("wrapped", _syntax_error_with_cause(make_error(), sql)),
        ("unwrapped", make_error()),
    )


def _save_with_template_error(client, sql, error, *, description):
    """Create a dataset with ``sql`` and save it while templating raises
    ``error``. Returns the dataset pk and the response."""
    pk = _make_virtual_dataset(sql)
    with patch(GET_TEMPLATE_PROCESSOR, return_value=_processor_raising(error)):
        resp = _save(client, pk, description=description)
    return pk, resp


# ---------------------------------------------------------------------------
# Best-effort rows: the save returns 200 and the edit is persisted.
# ---------------------------------------------------------------------------
def test_save_is_best_effort_on_undefined_template_variable(client):
    """A template that touches a variable with no value at save time
    (from_dttm only exists when a chart runs the query) must not fail the
    save. The SQL goes through the real Jinja processor."""
    pk = _make_virtual_dataset(
        "SELECT '{{ from_dttm.isoformat() }}' AS ds, 1 AS amount",
        columns=EXISTING_COLUMNS,
    )
    with _template_processing_enabled():
        resp = _save(client, pk, description="undef-save")
    _assert_best_effort_save(
        pk, resp, "undef-save",
        "a Jinja dataset that references an undefined template variable",
    )


def test_save_is_best_effort_on_jinja_marked_parse_failure(client):
    """SQL that contains Jinja markers and renders, with no runtime context,
    to something the parser rejects must not fail the save. That SQL only
    resolves at query time."""
    pk = _make_virtual_dataset(
        "SELECT INVALID SYNTAX FROM {{ some_table }}", columns=EXISTING_COLUMNS
    )
    resp = _save(client, pk, description="jinja-parse-save")
    _assert_best_effort_save(
        pk, resp, "jinja-parse-save",
        "a Jinja-templated dataset whose rendered SQL fails to parse",
    )


# ---------------------------------------------------------------------------
# Hard-failure rows: the save returns an error status (>= 400).
# ---------------------------------------------------------------------------
def test_save_still_hard_errors_on_static_sql_parse_failure(client):
    """A parse failure on plain static SQL (no Jinja markers) is a real defect
    in the SQL, so the save must still fail."""
    pk = _make_virtual_dataset("SELECT INVALID SYNTAX FROM")
    resp = _save(client, pk, description="static-bad")
    assert resp.status_code >= 400, (
        "saving a plain-SQL dataset with invalid SQL must still "
        f"hard-error, got {resp.status_code}: {resp.data[:300]!r}"
    )


def test_save_still_hard_errors_on_template_security_violation(client):
    """A template failure caused by a sandbox/security violation, not a
    missing variable, must still fail the save in both error shapes. The SQL
    has Jinja markers, so only the kind of failure tells it apart from the
    best-effort rows."""
    sql = "SELECT {{ some_disallowed_attr }}"
    for shape, error in _template_error_shapes(
        lambda: SecurityError("access to attribute is unsafe"), sql
    ):
        _, resp = _save_with_template_error(
            client, sql, error, description=f"sec-{shape}"
        )
        assert resp.status_code >= 400, (
            f"a sandbox/security violation during templating ({shape}) must "
            f"still fail the save, got {resp.status_code}: {resp.data[:300]!r}"
        )


def test_save_still_hard_errors_on_genuine_db_error(client):
    """A generic DB error while reading the dataset's columns (connection,
    driver, or permission failure) must still fail the save, even when the
    dataset's SQL contains Jinja markers."""
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
        "a DB error while reading columns must still fail the save, "
        f"even on a Jinja dataset, got {resp.status_code}: {resp.data[:300]!r}"
    )


# ---------------------------------------------------------------------------
# Unaffected paths: plain valid SQL saves cleanly; validation paths unchanged.
# ---------------------------------------------------------------------------
def test_save_succeeds_on_plain_valid_sql(client):
    """A plain, valid virtual-dataset SQL still saves cleanly (200)."""
    pk = _make_virtual_dataset("SELECT 1 AS a")
    resp = _save(client, pk, description="plain-ok")
    assert resp.status_code == 200, (
        f"saving a plain valid dataset must succeed, got {resp.status_code}: "
        f"{resp.data[:300]!r}"
    )
    assert _persisted_description(pk) == "plain-ok"


def test_save_on_unknown_dataset_returns_not_found(client):
    """Saving an unknown dataset id still returns 404."""
    resp = _save(client, 999_999, description="nope")
    assert resp.status_code == 404, (
        f"unknown dataset id must return 404, got {resp.status_code}: "
        f"{resp.data[:300]!r}"
    )
