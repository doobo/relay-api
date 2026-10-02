/**
 * ES-module access to the vendored TweetNaCl bundle (/js/vendor/tweetnacl.js).
 *
 * TweetNaCl ships as a UMD file: loaded as a module it finds no `module` export
 * and assigns itself to globalThis.nacl as a side effect. Importing it for that
 * effect and re-exporting keeps the UMD quirk out of the call sites.
 *
 * Only the WebCrypto-free login fallback needs this, so auth.js dyn-imports it
 * lazily instead of paying ~32 KB on every page load.
 */
import "../vendor/tweetnacl.js";

export const nacl = globalThis.nacl;
