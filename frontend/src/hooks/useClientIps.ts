import { useEffect, useRef, useState } from 'react';
import { HttpUtil } from '@/utils';
import { normalizeClientIps, type ClientIpInfo } from '@/lib/clients/ip-log';

interface ApiMsg<T = unknown> {
  success?: boolean;
  obj?: T;
}

// Manage client IP logs and on-demand ip-api geolocation lookups when the log modal is open.
export function useClientIps(email: string | undefined, ipsModalOpen: boolean) {
  const [clientIps, setClientIps] = useState<ClientIpInfo[]>([]);
  const [ipsLoading, setIpsLoading] = useState(false);
  const [ipsClearing, setIpsClearing] = useState(false);
  const [ipLocations, setIpLocations] = useState<Record<string, string>>({});
  const [locationsLoading, setLocationsLoading] = useState(false);
  const queriedIpsRef = useRef<Set<string>>(new Set());
  const resolvedLocationsRef = useRef<Record<string, string>>({});
  const activeFetchCountRef = useRef(0);

  async function loadIps() {
    if (!email) return;
    setIpsLoading(true);
    try {
      const msg = (await HttpUtil.post(
        `/panel/api/clients/ips/${encodeURIComponent(email)}`,
      )) as ApiMsg<unknown[]>;
      if (!msg?.success) {
        setClientIps([]);
        return;
      }
      const normalized = normalizeClientIps(msg.obj);
      for (const entry of normalized) {
        if (!entry.location && !resolvedLocationsRef.current[entry.ip]) {
          queriedIpsRef.current.delete(entry.ip);
        }
      }
      setClientIps(normalized);
    } finally {
      setIpsLoading(false);
    }
  }

  async function clearIps() {
    if (!email) return;
    setIpsClearing(true);
    try {
      const msg = (await HttpUtil.post(
        `/panel/api/clients/clearIps/${encodeURIComponent(email)}`,
      )) as ApiMsg;
      if (msg?.success) setClientIps([]);
    } finally {
      setIpsClearing(false);
    }
  }

  function resetIps() {
    setClientIps([]);
    setLocationsLoading(false);
  }

  useEffect(() => {
    if (!ipsModalOpen || clientIps.length === 0) return;

    const missingSet = new Set<string>();
    for (const entry of clientIps) {
      if (
        !entry.location &&
        !resolvedLocationsRef.current[entry.ip] &&
        !queriedIpsRef.current.has(entry.ip)
      ) {
        missingSet.add(entry.ip);
      }
    }
    const missing = Array.from(missingSet);
    if (missing.length === 0) return;

    const controller = new AbortController();
    let cancelled = false;

    void (async () => {
      // Defer past synchronous effect body to avoid cascading setState and StrictMode pre-cleanup abort.
      await Promise.resolve();
      if (cancelled) return;
      missing.forEach((ip) => queriedIpsRef.current.add(ip));
      activeFetchCountRef.current += 1;
      setLocationsLoading(true);
      try {
        const msg = (await HttpUtil.post(
          '/panel/api/clients/ipLocations',
          { ips: missing },
          {
            headers: { 'Content-Type': 'application/json' },
            silent: true,
            signal: controller.signal,
          },
        )) as ApiMsg<Record<string, unknown>>;
        if (cancelled) return;
        if (msg?.success && msg.obj && typeof msg.obj === 'object') {
          const next: Record<string, string> = {};
          for (const [ip, loc] of Object.entries(msg.obj)) {
            if (typeof loc === 'string' && loc.length > 0) {
              next[ip] = loc;
            }
          }
          if (Object.keys(next).length > 0) {
            resolvedLocationsRef.current = { ...resolvedLocationsRef.current, ...next };
            setIpLocations(resolvedLocationsRef.current);
          }
        }
      } finally {
        if (cancelled) {
          missing.forEach((ip) => queriedIpsRef.current.delete(ip));
        }
        activeFetchCountRef.current = Math.max(0, activeFetchCountRef.current - 1);
        setLocationsLoading(activeFetchCountRef.current > 0);
      }
    })();

    return () => {
      cancelled = true;
      controller.abort();
    };
  }, [ipsModalOpen, clientIps]);

  return {
    clientIps,
    ipsLoading,
    ipsClearing,
    ipLocations,
    locationsLoading,
    loadIps,
    clearIps,
    resetIps,
  };
}
