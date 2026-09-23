# The custom domain, e.g. owls.eastsideaudubon.org. Optional: with
# `custom_domain` empty nothing here exists and the app answers only on its own
# `<app>.<region>.azurecontainerapps.io` name.
#
# Three things have to happen, in this order, and only the middle one is
# Terraform's (DEPLOYMENT.md, *Custom domain*, is the prose version):
#
#   1. A CNAME to the app and an `asuid.<name>` TXT record carrying the app's
#      verification id have to resolve *publicly*. Our DNS is hosted outside
#      Azure, so those two records are made by hand there and nothing in this
#      stack knows about them -- but they have to exist before the apply that
#      creates the resource below, because Azure checks them while it creates
#      it. A hosts file can't stand in for them.
#   2. The hostname is added to the app (here). It arrives *unbound*: there is
#      no certificate yet, so the name resolves but doesn't serve HTTPS.
#   3. `az containerapp hostname bind` issues the free managed certificate and
#      binds it; Azure renews it from then on. That is a command rather than a
#      resource because the provider can't point a custom domain at a managed
#      certificate -- see the lifecycle block below.
#
# Binding the name does not move the app onto it: `public_url` does that, and
# is deliberately a later step, after the provider has the redirect URI.

resource "azurerm_container_app_custom_domain" "this" {
  count            = var.custom_domain != "" ? 1 : 0
  name             = var.custom_domain
  container_app_id = azurerm_container_app.this.id

  lifecycle {
    # No certificate named here on purpose. Omitting one is what tells the
    # provider to leave the binding for an Azure managed certificate, which
    # `az containerapp hostname bind` then creates and which Azure renews;
    # these are the two fields that command fills in. Without ignoring them
    # every later plan would see the drift and want to replace the binding,
    # taking the site's certificate with it. The provider can't set them
    # itself: the writable field takes an environment *certificate* id, and a
    # managed certificate is a different resource type (`managedCertificates`),
    # which it refuses.
    ignore_changes = [
      certificate_binding_type,
      container_app_environment_certificate_id,
    ]
  }
}
