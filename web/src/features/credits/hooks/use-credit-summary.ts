/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { useQuery } from '@tanstack/react-query'
import { useEffect } from 'react'

import { useAuthStore } from '@/stores/auth-store'

import { creditQueryKeys, getCreditAccount } from '../api'

export function useCreditSummary(userId?: number) {
  const signedIn = useAuthStore((state) => state.auth.user?.id)
  const viewer = userId ?? signedIn
  const query = useQuery({
    queryKey: [...creditQueryKeys.account(undefined, 1), viewer],
    enabled: !!viewer,
    queryFn: () => getCreditAccount(),
    refetchInterval: 30000,
  })
  const { data, dataUpdatedAt, refetch } = query
  useEffect(() => {
    if (!data) return
    const deadlines = data.packs
      .flatMap((pack) => [pack.starts_at, pack.expires_at])
      .filter((time) => time > data.server_time)
    if (!deadlines.length) return
    const timer = setTimeout(
      () => void refetch(),
      Math.min(
        2147483647,
        Math.max(0, Math.min(...deadlines) * 1000 - data.server_time * 1000)
      )
    )
    return () => clearTimeout(timer)
  }, [data, dataUpdatedAt, refetch])
  return query
}
