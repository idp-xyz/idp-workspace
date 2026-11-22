/** the pattern of a connection id: the bridge's, idp-host's idp.host/authorization.required event's (its D61), and the server's. It
 * keeps addresses, paths and markup out of an id. */
export const CONNECTION_ID = /^[A-Za-z0-9_-]{1,64}$/;
