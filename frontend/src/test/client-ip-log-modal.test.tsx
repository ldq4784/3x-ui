import { describe, it, expect, vi } from 'vitest';
import { waitFor } from '@testing-library/react';
import { useEffect } from 'react';

import ClientIpLogModal from '@/components/clients/ClientIpLogModal';
import { useClientIps } from '@/hooks/useClientIps';
import { HttpUtil, Msg } from '@/utils';
import { renderWithProviders } from './test-utils';

function Harness({ open }: { open: boolean }) {
  const { clientIps, ipsLoading, ipsClearing, ipLocations, locationsLoading, loadIps, clearIps } =
    useClientIps('qk', open);

  useEffect(() => {
    void loadIps();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return (
    <ClientIpLogModal
      open={open}
      title="IP Log — qk"
      clientIps={clientIps}
      ipLocations={ipLocations}
      loading={ipsLoading}
      locationsLoading={locationsLoading}
      clearing={ipsClearing}
      onRefresh={() => void loadIps()}
      onClearAll={() => void clearIps()}
      onClose={() => {}}
    />
  );
}

describe('ClientIpLogModal and useClientIps', () => {
  it('renders city+ISP badge before IP tag and fetches missing locations only when opened', async () => {
    const postSpy = vi.spyOn(HttpUtil, 'post').mockImplementation(async (url) => {
      if (url === '/panel/api/clients/ips/qk') {
        return new Msg(true, '', [
          { ip: '221.6.10.213', time: '2026-11-13 21:55:40', node: 'GCP HK' },
          {
            ip: '[2409:8a55:32e2:e600:ad8d:9a81:9093:1c6d]',
            time: '2026-11-14 10:33:05',
            node: 'AWS IN',
          },
          { ip: '14.154.206.197', time: '2026-11-13 22:13:52', location: '深圳电信' },
        ]);
      }
      if (url === '/panel/api/clients/ipLocations') {
        return new Msg(true, '', {
          '221.6.10.213': '苏州联通',
          '[2409:8a55:32e2:e600:ad8d:9a81:9093:1c6d]': '深圳移动',
        });
      }
      return new Msg(true, '', {});
    });

    const { rerender } = renderWithProviders(<Harness open={false} />);

    await waitFor(() => {
      expect(postSpy).toHaveBeenCalledWith('/panel/api/clients/ips/qk');
    });
    expect(postSpy).not.toHaveBeenCalledWith(
      '/panel/api/clients/ipLocations',
      expect.anything(),
      expect.anything(),
    );

    rerender(<Harness open />);

    await waitFor(() => {
      expect(postSpy).toHaveBeenCalledWith(
        '/panel/api/clients/ipLocations',
        { ips: ['221.6.10.213', '[2409:8a55:32e2:e600:ad8d:9a81:9093:1c6d]'] },
        expect.objectContaining({
          headers: { 'Content-Type': 'application/json' },
          silent: true,
        }),
      );
    });

    await waitFor(() => {
      expect(document.body.textContent).toContain('苏州联通');
      expect(document.body.textContent).toContain('深圳移动');
      expect(document.body.textContent).toContain('深圳电信');
    });

    const tags = Array.from(document.querySelectorAll('.ant-tag')).map((el) =>
      (el.textContent ?? '').trim(),
    );
    const locIdx = tags.indexOf('苏州联通');
    const ipIdx = tags.findIndex((text) => text.startsWith('221.6.10.213'));
    expect(locIdx).toBeGreaterThanOrEqual(0);
    expect(ipIdx).toBeGreaterThan(locIdx);

    const v6LocIdx = tags.indexOf('深圳移动');
    const v6IpIdx = tags.findIndex((text) =>
      text.startsWith('[2409:8a55:32e2:e600:ad8d:9a81:9093:1c6d]'),
    );
    expect(v6LocIdx).toBeGreaterThanOrEqual(0);
    expect(v6IpIdx).toBeGreaterThan(v6LocIdx);
  });
});
