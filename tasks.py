"""Invoke tasks for the Python tooling (e2e tests, embedded scripts, docs prose).

Go code is managed through the Makefile; these tasks cover the Python, YAML and
Markdown linting standardised across OpsMill repositories. Run them inside the uv
environment, e.g. ``uv run invoke format`` / ``uv run invoke lint``.
"""

from pathlib import Path

from invoke import Context, task

MAIN_DIRECTORY_PATH = Path(__file__).parent


@task(name="format")
def format_all(ctx: Context) -> None:
    """Format Python (ruff) and Markdown (rumdl) files."""
    with ctx.cd(MAIN_DIRECTORY_PATH):
        ctx.run("ruff format .", pty=True)
        ctx.run("ruff check . --fix", pty=True)
        ctx.run("rumdl fmt .", pty=True)


@task
def lint_ruff(ctx: Context) -> None:
    """Check Python formatting and lint with ruff."""
    with ctx.cd(MAIN_DIRECTORY_PATH):
        ctx.run("ruff format --check --diff", pty=True)
        ctx.run("ruff check .", pty=True)


@task
def lint_mypy(ctx: Context) -> None:
    """Type-check Python with mypy."""
    with ctx.cd(MAIN_DIRECTORY_PATH):
        ctx.run("mypy .", pty=True)


@task
def lint_yaml(ctx: Context) -> None:
    """Lint YAML with yamllint."""
    with ctx.cd(MAIN_DIRECTORY_PATH):
        ctx.run("yamllint -s .", pty=True)


@task
def lint_markdown(ctx: Context) -> None:
    """Lint Markdown with rumdl."""
    with ctx.cd(MAIN_DIRECTORY_PATH):
        ctx.run("rumdl check .", pty=True)


@task(name="lint")
def lint_all(ctx: Context) -> None:
    """Run all linters (ruff, mypy, yamllint, rumdl)."""
    lint_ruff(ctx)
    lint_mypy(ctx)
    lint_yaml(ctx)
    lint_markdown(ctx)
