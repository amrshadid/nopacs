/** A list request: page_size and page_token are the fields the contract's list calls share. */
type PagedRequest = { pageToken?: string };

/** A list response: next_page_token is empty on the last page. */
type PagedResponse = { nextPageToken: string };

/** The element type of the array in field `K` of a response. */
type Item<Res, K extends keyof Res> = Res[K] extends readonly (infer I)[] ? I : never;

/**
 * Yields every item of a list call, page after page.
 *
 * Calls `call(request)`, yields the items of the response's repeated field
 * `field`, and calls again with the response's `nextPageToken` until the
 * server answers an empty one. Set `pageSize` on the request to choose the
 * page. The request object is not changed. A server that answers the token it
 * was just sent makes it throw rather than loop forever.
 *
 * @example
 * for await (const tenant of pages(clients.tenants.listTenants, { pageSize: 50 }, "tenants")) {
 *   console.log(tenant.name);
 * }
 */
export async function* pages<
  Req extends PagedRequest,
  Res extends PagedResponse,
  K extends keyof Res,
>(
  call: (request: Req) => Promise<Res>,
  request: Req,
  field: K,
): AsyncGenerator<Item<Res, K>, void, undefined> {
  let current = request;
  for (;;) {
    const response = await call(current);
    yield* response[field] as Iterable<Item<Res, K>>;
    if (!response.nextPageToken) return;
    if (response.nextPageToken === (current.pageToken ?? "")) {
      throw new Error(`the server repeated nextPageToken while listing ${String(field)}`);
    }
    current = { ...request, pageToken: response.nextPageToken };
  }
}
