interface GPSCoordinates {
  gps_latitude?: number | null
  gps_longitude?: number | null
}

// Match backend GPS eligibility: a single zero is valid; (0, 0) is a placeholder.
export function hasValidGPS<T extends GPSCoordinates>(
  photo: T | null | undefined,
): photo is T & { gps_latitude: number; gps_longitude: number } {
  return photo?.gps_latitude != null && photo.gps_longitude != null
    && (photo.gps_latitude !== 0 || photo.gps_longitude !== 0)
}
