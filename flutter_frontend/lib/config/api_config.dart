/// Use --dart-define=API_BASE_URL=http://192.168.1.10:8080 on a physical phone.
class ApiConfig {
  static const baseUrl = String.fromEnvironment(
    'API_BASE_URL',
    defaultValue: 'http://10.0.2.2:8080',
  );
}
