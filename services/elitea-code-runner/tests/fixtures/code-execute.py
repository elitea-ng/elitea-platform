#!/usr/local/bin/python
"""TEST ONLY: native Python adapter for supervisor lifecycle integration.

This is not the product language runtime or a Pyodide compatibility layer.
The test builds it into a disposable image, under the normal outer limits.
"""
import json
import sys

with open(sys.argv[1], encoding="utf-8") as stream:
    request = json.load(stream)
if request["language"] != "python":
    raise ValueError("fixture accepts Python only")
exec(compile(request["source"], "fixture-code.py", "exec"), {"elitea_state": request["input"]})
