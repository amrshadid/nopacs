"""Check a tenant's QIDO-RS searches with dicomweb-client against the answers
of the archive's own search.

Usage: search-dicomweb-client.py <DICOMweb root URL> <manifest.json>

The key is read from the environment variable NOPACS_DICOMWEB_KEY, never from
the command line, and sent as a bearer token. The manifest is
  {"queries": [{"name": <a name>,
                "kind": "studies" | "series" | "series-per-study",
                "filters": {<matching key>: <value>},
                "expect": [<UIDs the archive's search found>]}]}
where a "studies" query is a search of studies and expects Study Instance
UIDs, a "series" query a search of series in all studies and a
"series-per-study" query the same search made once in each study the tenant
holds, and both expect Series Instance UIDs.

With dicomweb-client, a client that is not NoPACs', it runs each query as
QIDO-RS (PS3.18 8.3.4) and requires the UIDs it gets back to be exactly the
ones the manifest expects, which `nopacs search` answered for the same
condition.

Exit status 0 when everything agrees; each difference is printed.
"""
import json
import os

from dicomweb_client.api import DICOMwebClient

STUDY, SERIES = "0020000D", "0020000E"


def uids(found, tag):
    """The UIDs of a QIDO-RS answer, by the tag that holds them."""
    return sorted(item[tag]["Value"][0] for item in found)


def run(client, q, studies):
    """The UIDs the query finds with dicomweb-client."""
    kind, filters = q["kind"], q["filters"]
    if kind == "studies":
        return uids(client.search_for_studies(search_filters=filters, get_remaining=True), STUDY)
    if kind == "series":
        return uids(client.search_for_series(search_filters=filters, get_remaining=True), SERIES)
    found = []
    for study in studies:
        found += uids(client.search_for_series(study_instance_uid=study, search_filters=filters, get_remaining=True), SERIES)
    return sorted(found)


def main(argv):
    url, manifest = argv[0], argv[1]
    with open(manifest) as f:
        queries = json.load(f)["queries"]
    client = DICOMwebClient(url=url, headers={"Authorization": "Bearer " + os.environ["NOPACS_DICOMWEB_KEY"]})
    studies = uids(client.search_for_studies(get_remaining=True), STUDY)
    problems = []
    for q in queries:
        try:
            got = run(client, q, studies)
        except Exception as e:  # the client raises its own errors for a refused search
            problems.append("%s: dicomweb-client failed: %s" % (q["name"], e))
            continue
        want = sorted(q["expect"])
        print("%-17s %3d found, %3d expected  %s %s" % (q["kind"], len(got), len(want), q["name"], "ok" if got == want else "DIFFER"))
        if got != want:
            problems.append("%s: QIDO-RS finds %s, `nopacs search` finds %s" % (q["name"], got, want))
    for p in problems:
        print("DIFFER", p)
    print("%d studies; %d queries checked; %d differ" % (len(studies), len(queries), len(problems)))
    return 1 if problems else 0
