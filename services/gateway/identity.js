// Identity comes only from the JWT that each service verifies (ADR 0001). The gateway refuses
// any client-sent X-User-* or X-Roles header (nginx.conf.template), so nobody can add a trusted-header
// shortcut later by accident. nginx can't match header names by pattern; njs can.

const IDENTITY_HEADER = /^(x-user-.*|x-roles)$/i

/** '1' when the request carries an identity header, '' otherwise (for js_set). */
function hasIdentityHeader(r) {
    return r.rawHeadersIn.some((h) => IDENTITY_HEADER.test(h[0])) ? '1' : ''
}

export default { hasIdentityHeader }
