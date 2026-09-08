// Canonical JSON serialization and sha256 digests.
//
// Canonical form: object keys sorted lexicographically (UTF-16 code
// unit order), no insignificant whitespace, arrays keep their order.
// Feeding the *parsed* value (not the source text) through this makes
// the digest invariant under reformatting and key reordering, which is
// the stability contract the dataset digest relies on.

import { createHash } from "node:crypto";

export class CanonicalizationError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "CanonicalizationError";
  }
}

export function canonicalize(value: unknown): string {
  if (value === null) {
    return "null";
  }
  switch (typeof value) {
    case "boolean":
      return value ? "true" : "false";
    case "number":
      if (!Number.isFinite(value)) {
        throw new CanonicalizationError(`non-finite number: ${value}`);
      }
      return JSON.stringify(value);
    case "string":
      return JSON.stringify(value);
    case "object": {
      if (Array.isArray(value)) {
        return `[${value.map(canonicalize).join(",")}]`;
      }
      const keys = Object.keys(value as Record<string, unknown>).sort();
      const body = keys
        .map((key) => `${JSON.stringify(key)}:${canonicalize((value as Record<string, unknown>)[key])}`)
        .join(",");
      return `{${body}}`;
    }
    default:
      throw new CanonicalizationError(
        `value of type ${typeof value} is not JSON data`,
      );
  }
}

/** sha256 over the canonical form, prefixed with the wire's `sha256:` marker. */
export function sha256Digest(value: unknown): string {
  return `sha256:${createHash("sha256")
    .update(canonicalize(value), "utf8")
    .digest("hex")}`;
}

/** Plain sha256 hex digest of a UTF-8 string. */
export function sha256Hex(text: string): string {
  return createHash("sha256").update(text, "utf8").digest("hex");
}

/** Deep JSON-value equality via canonical form (arrays ordered, keys unordered). */
export function jsonEquals(a: unknown, b: unknown): boolean {
  try {
    return canonicalize(a) === canonicalize(b);
  } catch {
    return false;
  }
}
