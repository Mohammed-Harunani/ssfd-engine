#!/usr/bin/env python3
"""Check SSFD XML against the pinned official ISO 20022 schemas."""

from argparse import ArgumentParser
from hashlib import sha256
from pathlib import Path
from urllib.request import urlopen

from lxml import etree

SCHEMAS = {
    "pain.001.001.09": (
        "https://www.iso20022.org/message/14346/download",
        "de038b373e47b0077b1832ddd81f4b2f1eb25d35721f62da1e38b7f5a09fda24",
    ),
    "camt.053.001.08": (
        "https://www.iso20022.org/message/12736/download",
        "338e9cb0c9989b5181802a7b773eece070d6815fc9d6483ac0579117bc24ccba",
    ),
}


def main():
    parser = ArgumentParser(description=__doc__)
    parser.add_argument("export_directory", type=Path)
    parser.add_argument("--schema-directory", type=Path)
    args = parser.parse_args()
    xml_parser = etree.XMLParser(resolve_entities=False, no_network=True)
    for message, (url, digest) in SCHEMAS.items():
        if args.schema_directory:
            raw = (args.schema_directory / f"{message}.xsd").read_bytes()
        else:
            with urlopen(url, timeout=30) as source:
                raw = source.read()
        if sha256(raw).hexdigest() != digest:
            raise SystemExit(f"Unexpected official schema digest for {message}")
        schema = etree.XMLSchema(etree.fromstring(raw, parser=xml_parser))
        doc = args.export_directory / f"{message}.xml"
        schema.assertValid(etree.parse(str(doc), parser=xml_parser))
        print(f"{doc}: valid")


if __name__ == "__main__":
    main()
