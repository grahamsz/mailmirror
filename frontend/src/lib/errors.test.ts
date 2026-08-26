// Tests for network-error classification: fetch's TypeError signature, browser
// message variants, and the rule that server answers are never network errors.

import { describe, expect, it } from "vitest";
import { ApiError } from "../api";
import { isNetworkError } from "./errors";

describe("isNetworkError", () => {
  it("treats fetch TypeErrors as connectivity failures", () => {
    expect(isNetworkError(new TypeError("Failed to fetch"))).toBe(true);
    expect(isNetworkError(new TypeError("NetworkError when attempting to fetch resource."))).toBe(true);
  });

  it("recognizes common browser offline messages", () => {
    expect(isNetworkError(new Error("Failed to fetch"))).toBe(true);
    expect(isNetworkError(new Error("Load failed"))).toBe(true);
    expect(isNetworkError(new Error("The Internet connection appears to be offline."))).toBe(true);
  });

  it("never classifies a decoded API error as a network failure", () => {
    expect(isNetworkError(new ApiError(500, "boom"))).toBe(false);
    expect(isNetworkError(new ApiError(401, "session expired"))).toBe(false);
  });

  it("leaves unrelated errors and unknown values alone", () => {
    expect(isNetworkError(new Error("Something else"))).toBe(false);
    expect(isNetworkError("Failed to fetch")).toBe(false);
    expect(isNetworkError(null)).toBe(false);
  });
});
