"""Dump recent Prefect events as JSON for the troubleshooting bundle.

The Prefect CLI has no non-streaming events command, so this script queries
the server's events API directly. It runs inside the task-manager container,
which ships httpx as a Prefect dependency. The events endpoint caps the page
size at 50, so recent events are gathered by following next_page links.
"""

import json
import os
import sys

import httpx

DEFAULT_API_URL = "http://localhost:4200/api"
PAGE_SIZE = 50
EVENT_LIMIT = 200
# Hard cap on pages followed. With PAGE_SIZE=50 and EVENT_LIMIT=200 four pages
# suffice; the bound stops a runaway loop if the API keeps returning a truthy
# next_page (FIX-9).
MAX_PAGES = 20


def main() -> int:
    api_url = os.environ.get("PREFECT_API_URL", DEFAULT_API_URL).rstrip("/")

    events = []
    total = 0
    with httpx.Client(timeout=30) as client:
        response = client.post(
            f"{api_url}/events/filter",
            json={"limit": PAGE_SIZE},
        )
        response.raise_for_status()
        page = response.json()
        total = page.get("total", 0)
        events.extend(page.get("events", []))

        pages = 1
        while len(events) < EVENT_LIMIT and page.get("next_page") and pages < MAX_PAGES:
            response = client.get(page["next_page"])
            response.raise_for_status()
            page = response.json()
            new_events = page.get("events", [])
            # A truthy next_page pointing at an empty page would otherwise spin
            # forever; stop as soon as a page yields nothing (FIX-9).
            if not new_events:
                break
            events.extend(new_events)
            pages += 1

    json.dump(
        {"total": total, "collected": len(events), "events": events[:EVENT_LIMIT]},
        sys.stdout,
        indent=2,
        default=str,
    )
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
