#!/usr/bin/env python3
"""The browser half of the viewer harness (scripts/prove/viewer.py).

It runs inside the Playwright container, not on the host: it opens each study
in the viewer served at plan["viewer"], waits until a viewport has drawn,
takes a screenshot of the largest canvas, and records every request the page
made to the archive (plan["api"]) with its status and Accept header, and how
many of them were open at once. It writes result.json and the screenshots to
the directory it is given and judges nothing: the host reads them with its
own tools.

  viewer_driver.py <directory holding plan.json>
"""

import json
import os
import sys
import time
import urllib.error
import urllib.request
from urllib.parse import urlparse

from playwright.sync_api import sync_playwright


def reachable(api, key):
    """Whether the archive answers from this container; a refusal or a 404
    still proves the address is right, a connection error does not."""
    req = urllib.request.Request(api.rstrip("/") + "/studies?limit=1", headers={"Authorization": "Bearer " + key})
    try:
        urllib.request.urlopen(req, timeout=15).read()
    except urllib.error.HTTPError:
        return True, ""
    except Exception as e:  # noqa: BLE001 - the text is the diagnosis
        return False, "%s: %s" % (type(e).__name__, e)
    return True, ""


def open_study(browser, plan, study, shots):
    """One study in a new browser context: its requests and its picture."""
    api_origin = "{0.scheme}://{0.netloc}".format(urlparse(plan["api"]))
    ctx = browser.new_context(viewport={"width": 1600, "height": 1000})
    page = ctx.new_page()
    reqs, inflight, state = {}, set(), {"peak": 0}
    console = []

    def is_api(r):
        return r.url.startswith(api_origin)

    def on_request(r):
        if not is_api(r):
            return
        reqs[id(r)] = {"method": r.method, "url": urlparse(r.url).path + ("?" + urlparse(r.url).query if urlparse(r.url).query else ""),
                       "accept": r.headers.get("accept", ""), "status": None, "failed": "", "t0": time.time()}
        inflight.add(id(r))
        state["peak"] = max(state["peak"], len(inflight))

    def on_response(r):
        e = reqs.get(id(r.request))
        if e is not None:
            e["status"] = r.status
            e["type"] = r.headers.get("content-type", "")

    def on_done(r):
        inflight.discard(id(r))

    def on_failed(r):
        e = reqs.get(id(r))
        if e is not None:
            e["failed"] = r.failure or "failed"
        inflight.discard(id(r))

    page.on("request", on_request)
    page.on("response", on_response)
    page.on("requestfinished", on_done)
    page.on("requestfailed", on_failed)
    page.on("console", lambda m: console.append(m.text[:300]) if m.type == "error" else None)

    out = {"name": study["name"], "uid": study["uid"], "canvas": None, "shot": "", "console_errors": console, "waited": 0.0}
    t0 = time.time()
    page.goto("%s/viewer?StudyInstanceUIDs=%s" % (plan["viewer"].rstrip("/"), study["uid"]), wait_until="domcontentloaded", timeout=60000)
    # A viewport has drawn when a canvas of a useful size exists and the
    # archive has been quiet for two seconds; the clock is the study's own.
    quiet_since = None
    while time.time() - t0 < plan["timeout"]:
        time.sleep(0.5)
        if inflight:
            quiet_since = None
            continue
        big = page.evaluate("""() => [...document.querySelectorAll('canvas')]
            .map(c => { const b = c.getBoundingClientRect(); return b.width * b.height; })
            .filter(a => a > 90000).length""")
        if big and quiet_since is None:
            quiet_since = time.time()
        if big and time.time() - quiet_since >= 2.0:
            break
    out["waited"] = round(time.time() - t0, 1)
    boxes = page.evaluate("""() => [...document.querySelectorAll('canvas')].map(c => {
        const b = c.getBoundingClientRect(); return {x: b.x, y: b.y, width: b.width, height: b.height}; })""")
    boxes = [b for b in boxes if b["width"] * b["height"] > 0]
    page.screenshot(path=os.path.join(shots, study["name"] + "-page.png"))
    if boxes:
        box = max(boxes, key=lambda b: b["width"] * b["height"])
        out["canvas"] = box
        out["shot"] = study["name"] + "-canvas.png"
        page.screenshot(path=os.path.join(shots, out["shot"]), clip=box)
    out["requests"] = [{k: v for k, v in e.items() if k != "t0"} for e in reqs.values()]
    out["peak_concurrent"] = state["peak"]
    ctx.close()
    return out


def main():
    d = sys.argv[1]
    with open(os.path.join(d, "plan.json")) as f:
        plan = json.load(f)
    ok, why = reachable(plan["api"], plan["key"])
    result = {"reachable": ok, "reachable_error": why, "studies": []}
    if ok:
        shots = os.path.join(d, "shots")
        os.makedirs(shots, exist_ok=True)
        with sync_playwright() as p:
            browser = p.chromium.launch(args=["--use-gl=swiftshader", "--enable-webgl", "--ignore-gpu-blocklist"])
            result["browser"] = browser.version
            for study in plan["studies"]:
                try:
                    result["studies"].append(open_study(browser, plan, study, shots))
                except Exception as e:  # noqa: BLE001 - one study failing is one entry
                    result["studies"].append({"name": study["name"], "uid": study["uid"], "error": "%s: %s" % (type(e).__name__, e)})
            browser.close()
    with open(os.path.join(d, "result.json"), "w") as f:
        json.dump(result, f, indent=1)


if __name__ == "__main__":
    main()
