"""Compile the API contract with Python's grpcio-tools, a second toolchain.

buf and protoc-gen-go are one toolchain; a contract that only they accept may
be shaped by them. This script compiles the same proto files with the protoc
that grpcio-tools bundles, imports the generated Python modules, and prints
every service and method they define. It fails when a file does not compile,
when no service is found, or when a method's comment lacks one of the three
parts the contract requires of it, each a line that starts with its label and
has text after it: the role it needs ("Role:"), what it writes to the audit
log ("Audit:") and the error kinds it can answer ("Errors:").

Usage: proto-python.py [proto root, default api/proto]

The pinned grpcio-tools version is in scripts/requirements/proto.txt.
"""

import importlib
import importlib.resources
import pathlib
import re
import sys
import tempfile

from google.protobuf import descriptor_pb2
from grpc_tools import protoc

from nopacs_scripts.report import Failure

# The parts every method's comment must have: a line that starts with the
# label, followed by text, so an empty "Role:" does not pass.
REQUIRED_PARTS = {
    label: re.compile(r"^\s*" + re.escape(label) + r"[ \t]*\S", re.MULTILINE)
    for label in ("Role:", "Audit:", "Errors:")
}

# Field numbers in descriptor.proto, used to read comments from the source
# information: FileDescriptorProto.service is 6, ServiceDescriptorProto.method
# is 2.
FILE_SERVICE = 6
SERVICE_METHOD = 2


def main(argv) -> int:
    root = pathlib.Path(argv[0] if argv else "api/proto").resolve()
    protos = sorted(str(p.relative_to(root)) for p in root.rglob("*.proto"))
    if not protos:
        print(f"no proto files under {root}", file=sys.stderr)
        return 1

    # grpcio-tools carries the well-known types (timestamp, duration) itself.
    wkt = importlib.resources.files("grpc_tools") / "_proto"

    with tempfile.TemporaryDirectory() as out:
        descriptor_set = pathlib.Path(out) / "contract.binpb"
        args = [
            "grpc_tools.protoc",
            f"-I{root}",
            f"-I{wkt}",
            f"--python_out={out}",
            f"--grpc_python_out={out}",
            f"--descriptor_set_out={descriptor_set}",
            "--include_source_info",
            *protos,
        ]
        print("grpc_tools.protoc", *protos)
        if protoc.main(args) != 0:
            print("grpc_tools.protoc failed", file=sys.stderr)
            return 1

        sys.path.insert(0, out)
        services = list_services(protos)
        if not services:
            print("the generated modules define no service", file=sys.stderr)
            return 1

        missing = check_method_comments(descriptor_set)

    for service, methods in services:
        print(service)
        for method in methods:
            print(f"  {method}")
    print(f"{len(services)} services, {sum(len(m) for _, m in services)} methods")

    for problem in missing:
        print(problem, file=sys.stderr)
    return 1 if missing else 0


def list_services(protos):
    """Import each generated module and answer its services and methods."""
    services = []
    for proto in protos:
        module_name = proto.removesuffix(".proto").replace("/", ".")
        pb2 = importlib.import_module(module_name + "_pb2")
        grpc_module = importlib.import_module(module_name + "_pb2_grpc")
        for service in pb2.DESCRIPTOR.services_by_name.values():
            # The client stub is what a Python caller uses; its absence would
            # mean the gRPC plugin did not understand the service.
            if not hasattr(grpc_module, service.name + "Stub"):
                raise Failure(f"{module_name}_pb2_grpc has no {service.name}Stub")
            methods = [
                f"{m.name}({m.input_type.name}) returns ({m.output_type.name})"
                f" [{idempotency(m)}]"
                for m in service.methods
            ]
            services.append((service.full_name, methods))
    return services


def check_method_comments(descriptor_set):
    """Answer one line for each method whose comment lacks a required part."""
    files = descriptor_pb2.FileDescriptorSet.FromString(descriptor_set.read_bytes())
    problems = []
    for file in files.file:
        comments = {
            tuple(loc.path): loc.leading_comments for loc in file.source_code_info.location
        }
        for si, service in enumerate(file.service):
            for mi, method in enumerate(service.method):
                text = comments.get((FILE_SERVICE, si, SERVICE_METHOD, mi), "")
                for label, pattern in REQUIRED_PARTS.items():
                    if not pattern.search(text):
                        problems.append(
                            f"{file.package}.{service.name}.{method.name}: "
                            f"its comment has no {label!r} line with text after it"
                        )
    return problems


def idempotency(method):
    """Answer the method's idempotency level by name, as the proto file sets it."""
    level = method.GetOptions().idempotency_level
    return descriptor_pb2.MethodOptions.IdempotencyLevel.Name(level)

