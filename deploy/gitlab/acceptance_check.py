#!/usr/bin/env python3
"""One-shot acceptance checks for the provisioned GitLab sandbox."""
import json
import os
import urllib.request

API = "http://127.0.0.1:8181/api/v4"
PAT = open(os.path.join(os.path.dirname(__file__), ".root-pat")).read().strip()


def get(path):
    req = urllib.request.Request(f"{API}{path}", headers={"PRIVATE-TOKEN": PAT})
    with urllib.request.urlopen(req) as r:
        return json.load(r)


def show(label, path, fmt):
    print(f"=== {label} ===")
    for item in get(path):
        print(" ", fmt(item))


show("dev-flow pipelines", "/projects/1/pipelines?per_page=3",
     lambda p: f'#{p["id"]} {p["status"]} source={p["source"]}')

show("dev-flow #6 jobs", "/projects/1/pipelines/6/jobs",
     lambda j: f'{j["status"]:>10}  {j["name"]}  runner={(j.get("runner") or {}).get("description", "-")}')

show("dev-flow #6 bridges (promote -> downstream)", "/projects/1/pipelines/6/bridges",
     lambda b: f'{b["status"]:>10}  {b["name"]} -> downstream '
               f'#{b["downstream_pipeline"]["id"]} ({b["downstream_pipeline"]["status"]})'
     if b.get("downstream_pipeline") else f'{b["status"]:>10}  {b["name"]} (no downstream)')

show("test-flow pipelines", "/projects/2/pipelines?per_page=5",
     lambda p: f'#{p["id"]} {p["status"]} source={p["source"]}')

latest_test = get("/projects/2/pipelines?per_page=1")[0]
show(f'test-flow #{latest_test["id"]} jobs', f'/projects/2/pipelines/{latest_test["id"]}/jobs',
     lambda j: f'{j["status"]:>10}  {j["name"]}')

print("=== protected branches (both projects) ===")
for pid in (1, 2):
    for b in get(f"/projects/{pid}/protected_branches"):
        push = [a.get("access_level_description") or a.get("access_level") for a in b.get("push_access_levels", [])]
        merge = [a.get("access_level_description") or a.get("access_level") for a in b.get("merge_access_levels", [])]
        print(f"  proj{pid} {b['name']}: push={push} merge={merge}")

print("=== runners ===")
for r in get("/runners/all?type=instance_type"):
    print(f'  #{r["id"]} {r["description"]} online={r.get("online")} paused={r.get("paused")}')

print("=== docker-executor evidence (job trace) ===")
jobs = get("/projects/1/jobs?per_page=5")
jid = jobs[0]["id"]
req = urllib.request.Request(f"{API}/projects/1/jobs/{jid}/trace", headers={"PRIVATE-TOKEN": PAT})
with urllib.request.urlopen(req) as r:
    trace = r.read().decode("utf-8", "replace")
for line in trace.splitlines():
    if "Running on" in line or "executor" in line.lower():
        print("  " + line.strip()[:100])
        break
