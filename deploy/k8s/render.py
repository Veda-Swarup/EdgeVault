#!/usr/bin/env python3
"""Render the Kustomize base using outputs from the Terraform state."""

import json
import os
import subprocess
import sys


def run(args):
    return subprocess.run(args, check=True, text=True, capture_output=True).stdout


try:
    outputs = json.loads(run(["terraform", "-chdir=deploy/terraform", "output", "-json"]))
    rendered = run(["kubectl", "kustomize", "deploy/k8s/base"])
    image = os.environ["EDGEVAULT_IMAGE"]
except (KeyError, subprocess.CalledProcessError) as exc:
    print(f"render: {exc}; set EDGEVAULT_IMAGE after pushing an image", file=sys.stderr)
    sys.exit(2)

values = {
    "__EDGEVAULT_AWS_REGION__": outputs["aws_region"]["value"],
    "__EDGEVAULT_CHUNKS_BUCKET__": outputs["chunks_bucket"]["value"],
    "__EDGEVAULT_SNAPSHOTS_BUCKET__": outputs["snapshots_bucket"]["value"],
    "__EDGEVAULT_KMS_KEY_ARN__": outputs["kms_key_arn"]["value"],
    "__EDGEVAULT_EDGE_ROLE_ARN__": outputs["edge_role_arn"]["value"],
    "__EDGEVAULT_GC_ROLE_ARN__": outputs["gc_role_arn"]["value"],
    "__EDGEVAULT_IMAGE__": image,
}
for key, value in values.items():
    rendered = rendered.replace(key, value)
if "__EDGEVAULT_" in rendered:
    print("render: unresolved deployment placeholder", file=sys.stderr)
    sys.exit(2)
sys.stdout.write(rendered)
