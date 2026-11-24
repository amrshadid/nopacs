import type { DescMethod, DescService } from "@bufbuild/protobuf";
import { createClient, type CallOptions, type Client, type Interceptor, type Transport } from "@connectrpc/connect";

import { AccessService } from "./gen/nopacs/v1/access_pb.js";
import { AuditService } from "./gen/nopacs/v1/audit_pb.js";
import { AuditSealService } from "./gen/nopacs/v1/audit_seal_pb.js";
import { BackupService } from "./gen/nopacs/v1/backup_pb.js";
import { CatalogService } from "./gen/nopacs/v1/catalog_pb.js";
import { CorrectionService } from "./gen/nopacs/v1/correction_pb.js";
import { DataService } from "./gen/nopacs/v1/data_pb.js";
import { DeidService } from "./gen/nopacs/v1/deid_pb.js";
import { JobService } from "./gen/nopacs/v1/job_pb.js";
import { MetadataService } from "./gen/nopacs/v1/metadata_pb.js";
import { MonitorService } from "./gen/nopacs/v1/monitor_pb.js";
import { ProfileService } from "./gen/nopacs/v1/profile_pb.js";
import { QuarantineService } from "./gen/nopacs/v1/quarantine_pb.js";
import { ReceiverService } from "./gen/nopacs/v1/receiver_pb.js";
import { ReceptionService } from "./gen/nopacs/v1/reception_pb.js";
import { ReplicaService } from "./gen/nopacs/v1/replica_pb.js";
import { RetentionService } from "./gen/nopacs/v1/retention_pb.js";
import { SenderService } from "./gen/nopacs/v1/sender_pb.js";
import { SettingsService } from "./gen/nopacs/v1/settings_pb.js";
import { StorageService } from "./gen/nopacs/v1/storage_pb.js";
import { TenantService } from "./gen/nopacs/v1/tenant_pb.js";
import { NopacsError } from "./errors.js";

/** One typed client for each service of the API contract. */
export interface NopacsClients {
  /** Keys, grants and who the caller is. */
  readonly access: Client<typeof AccessService>;
  /** The audit log. */
  readonly audit: Client<typeof AuditService>;
  /** The audit log's signed checkpoints: verify, export, the public key. */
  readonly auditSeal: Client<typeof AuditSealService>;
  /** Encrypted backups of the installation and exports of a tenant. */
  readonly backups: Client<typeof BackupService>;
  /** Subjects, sessions and scans of a tenant. */
  readonly catalog: Client<typeof CatalogService>;
  /** Deletes, relabels and replaces of archived data, and scrub findings. */
  readonly corrections: Client<typeof CorrectionService>;
  /** Pixel and other bulk data of a tenant. */
  readonly data: Client<typeof DataService>;
  /** The forward lookup of a value's codes, and the lookup tables of a tenant. */
  readonly deid: Client<typeof DeidService>;
  /** Jobs of a tenant, such as sending a session to a sender or importing a directory, and their failures. */
  readonly jobs: Client<typeof JobService>;
  /** DICOM metadata of a tenant. */
  readonly metadata: Client<typeof MetadataService>;
  /** Health and counters of the installation. */
  readonly monitor: Client<typeof MonitorService>;
  /** De-identification profiles of a tenant and the bases they build on. */
  readonly profiles: Client<typeof ProfileService>;
  /** Files set aside with a reason. */
  readonly quarantine: Client<typeof QuarantineService>;
  /** The receivers of a tenant. */
  readonly receivers: Client<typeof ReceiverService>;
  /** Receptions in progress or finished. */
  readonly receptions: Client<typeof ReceptionService>;
  /** Replica storages: their state, repair, recall and the switch. */
  readonly replicas: Client<typeof ReplicaService>;
  /** Retention rules, due sessions, disposal and holds. */
  readonly retention: Client<typeof RetentionService>;
  /** The senders of a tenant: the destinations its data may be sent to. */
  readonly senders: Client<typeof SenderService>;
  /** Settings of the installation and of its tenants. */
  readonly settings: Client<typeof SettingsService>;
  /** The storages of a tenant. */
  readonly storages: Client<typeof StorageService>;
  /** Tenants. */
  readonly tenants: Client<typeof TenantService>;
}

/**
 * A key is the visible ASCII characters, with no space: anything else cannot
 * be an HTTP header value, and the transport's error for it would repeat the
 * key.
 */
const KEY_PATTERN = /^[\x21-\x7e]+$/;

/**
 * Makes an interceptor that sends the key with every call as
 * `Authorization: Bearer <key>`, the credential the server reads. It throws a
 * `TypeError`, whose message never repeats the key, when the key is empty or
 * has a character a header cannot carry.
 */
export function keyInterceptor(key: string): Interceptor {
  if (!key) throw new TypeError("a key is required");
  if (!KEY_PATTERN.test(key)) {
    throw new TypeError("the key has a character that is not visible ASCII (no spaces or line breaks)");
  }
  const value = `Bearer ${key}`;
  return (next) => (req) => {
    req.header.set("authorization", value);
    return next(req);
  };
}

/**
 * An interceptor that turns every failure into a {@link NopacsError}, for a
 * unary call and for a server stream at the message that fails.
 */
export const errorInterceptor: Interceptor = (next) => async (req) => {
  let res;
  try {
    res = await next(req);
  } catch (err) {
    throw NopacsError.wrap(err);
  }
  if (!res.stream) return res;
  const messages = res.message;
  return {
    ...res,
    message: (async function* () {
      try {
        yield* messages;
      } catch (err) {
        throw NopacsError.wrap(err);
      }
    })(),
  };
};

/**
 * The interceptors a transport needs for {@link createClients}: the key and
 * the error conversion. Pass them to a transport you build yourself.
 */
export function nopacsInterceptors(key: string): Interceptor[] {
  return [errorInterceptor, keyInterceptor(key)];
}

/**
 * Whether a host names this machine: `localhost`, 127.0.0.0/8 or `::1`.
 * `hostname` is a URL's, so an IPv6 address has its brackets.
 */
function isLoopback(hostname: string): boolean {
  const host = hostname.replace(/^\[|\]$/g, "").toLowerCase();
  return host === "localhost" || host === "::1" || /^127(\.\d{1,3}){3}$/.test(host);
}

/**
 * Throws a `TypeError` when the server's address is plain `http:` on a host
 * that is not this machine, because the key would then travel in clear.
 * `allowInsecure` is the caller's explicit choice to send it anyway.
 */
export function assertSecure(baseUrl: string, allowInsecure: boolean | undefined): void {
  if (allowInsecure) return;
  const url = new URL(baseUrl);
  if (url.protocol === "http:" && !isLoopback(url.hostname)) {
    throw new TypeError(
      `${url.origin} is plain HTTP and not this machine, so the key would travel in clear; use https, or pass allowInsecure: true`,
    );
  }
}

/** The deadline, in milliseconds, of a unary call made without its own: 60 s; 0 means none. */
export const DEFAULT_TIMEOUT_MS = 60_000;

/** What {@link createClients} takes besides the transport. */
export interface ClientsOptions {
  /**
   * Milliseconds a unary call may take when the call does not say; 60000
   * when absent, 0 for no limit. A server stream has no deadline unless the
   * call passes `timeoutMs`, because a stream such as `monitor.watch` is
   * meant to stay open.
   */
  timeoutMs?: number;
}

/**
 * One service's client in which every method throws only {@link NopacsError}:
 * the deadline and cancellation errors Connect raises outside the
 * interceptors become one too, and a unary call gets the default deadline.
 */
function guarded<S extends DescService>(service: S, transport: Transport, timeoutMs: number | undefined): Client<S> {
  const inner = createClient(service, transport) as unknown as Record<string, (...args: unknown[]) => unknown>;
  const out: Record<string, unknown> = {};
  for (const method of service.methods as readonly DescMethod[]) {
    const call = inner[method.localName]!;
    const streams = method.methodKind === "server_streaming" || method.methodKind === "bidi_streaming";
    if (streams) {
      out[method.localName] = (input: unknown, options?: CallOptions) =>
        (async function* () {
          try {
            yield* call(input, options) as AsyncIterable<unknown>;
          } catch (err) {
            throw NopacsError.wrap(err);
          }
        })();
    } else {
      out[method.localName] = async (input: unknown, options?: CallOptions) => {
        const withDeadline =
          method.methodKind === "unary" && timeoutMs && options?.timeoutMs === undefined
            ? { ...options, timeoutMs }
            : options;
        try {
          return await call(input, withDeadline);
        } catch (err) {
          throw NopacsError.wrap(err);
        }
      };
    }
  }
  return out as Client<S>;
}

/**
 * Makes one client for each service, all over the given transport. Every
 * method throws only {@link NopacsError}, and a unary call has the default
 * deadline of `options.timeoutMs`.
 */
export function createClients(transport: Transport, options: ClientsOptions = {}): NopacsClients {
  const timeoutMs = options.timeoutMs ?? DEFAULT_TIMEOUT_MS;
  const make = <S extends DescService>(service: S) => guarded(service, transport, timeoutMs);
  return {
    access: make(AccessService),
    audit: make(AuditService),
    auditSeal: make(AuditSealService),
    backups: make(BackupService),
    catalog: make(CatalogService),
    corrections: make(CorrectionService),
    data: make(DataService),
    deid: make(DeidService),
    jobs: make(JobService),
    metadata: make(MetadataService),
    monitor: make(MonitorService),
    profiles: make(ProfileService),
    quarantine: make(QuarantineService),
    receivers: make(ReceiverService),
    receptions: make(ReceptionService),
    replicas: make(ReplicaService),
    retention: make(RetentionService),
    senders: make(SenderService),
    settings: make(SettingsService),
    storages: make(StorageService),
    tenants: make(TenantService),
  };
}
