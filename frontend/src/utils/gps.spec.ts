import { describe, expect, it } from 'vitest'
import { hasValidGPS } from './gps'

describe('hasValidGPS', () => {
  it.each([
    [undefined, false],
    [null, false],
    [{}, false],
    [{ gps_latitude: null, gps_longitude: 120 }, false],
    [{ gps_latitude: 30, gps_longitude: null }, false],
    [{ gps_latitude: 0, gps_longitude: 0 }, false],
    [{ gps_latitude: 0, gps_longitude: 120 }, true],
    [{ gps_latitude: 30, gps_longitude: 0 }, true],
    [{ gps_latitude: 30, gps_longitude: 120 }, true],
  ])('classifies %j as %s', (photo, expected) => {
    expect(hasValidGPS(photo)).toBe(expected)
  })
})
