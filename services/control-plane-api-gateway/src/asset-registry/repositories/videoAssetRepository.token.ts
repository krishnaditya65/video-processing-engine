/**
 * @file videoAssetRepository.token.ts
 * TypeScript interfaces have no runtime representation, so NestJS dependency
 * injection needs a concrete token to bind {@link VideoAssetRepositoryInterface}
 * implementations to. This symbol is that token — inject it via
 * `@Inject(VIDEO_ASSET_REPOSITORY)`. Swapping the bound implementation (in
 * {@link AssetRegistryModule}) is the entire "swap the Repository pattern's backing
 * store" story for this service.
 */

export const VIDEO_ASSET_REPOSITORY = Symbol('VIDEO_ASSET_REPOSITORY');
